package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.etcd.io/bbolt"
)

const (
	AIStatusPending = "pending"
	AIStatusSuccess = "succeeded"
	AIStatusFailed  = "failed"
)

var (
	recordsBucket  = []byte("records")
	errRecordFound = errors.New("record found")
)

type Store struct {
	db *bbolt.DB
}

type Record struct {
	SourcePath    string    `json:"source_path"`
	SHA256        string    `json:"sha256"`
	SourceModTime time.Time `json:"source_mod_time"`
	SourceSize    int64     `json:"source_size"`
	VaultPDFPath  string    `json:"vault_pdf_path"`
	VaultNotePath string    `json:"vault_note_path"`
	ImportedAt    time.Time `json:"imported_at"`

	// AIStatus is persisted as status. Records written before the asynchronous
	// queue used ai_status (or no status at all); UnmarshalJSON handles both.
	AIStatus      string    `json:"status"`
	AIRetryCount  int       `json:"ai_retry_count"`
	AILastError   string    `json:"ai_last_error"`
	AILastRetryAt time.Time `json:"ai_last_retry_at"`
	// AILastSuccessAt records when AI last completed successfully for this
	// record. Used by the reprocess-debounce feature to avoid re-running AI
	// on BOOX wrapper-only PDF rewrites. Zero value for legacy records or
	// records that never had a successful AI run.
	AILastSuccessAt time.Time `json:"ai_last_success_at"`
}

// UnmarshalJSON accepts the former ai_status field and treats records without
// either status as completed. This keeps pre-queue databases from being
// accidentally enqueued after upgrade.
func (r *Record) UnmarshalJSON(data []byte) error {
	type recordAlias Record
	var raw struct {
		recordAlias
		LegacyAIStatus string `json:"ai_status"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*r = Record(raw.recordAlias)
	if r.AIStatus == "" {
		if raw.LegacyAIStatus != "" {
			r.AIStatus = raw.LegacyAIStatus
		} else {
			r.AIStatus = AIStatusSuccess
		}
	}
	// The original synchronous implementation used "success".
	if r.AIStatus == "success" {
		r.AIStatus = AIStatusSuccess
	}
	return nil
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open state db %s: %w", path, err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(recordsBucket); err != nil {
			return err
		}
		return nil
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init state db %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) GetBySourcePath(p string) (*Record, error) {
	var out *Record
	err := s.db.View(func(tx *bbolt.Tx) error {
		v := tx.Bucket(recordsBucket).Get([]byte(p))
		if v == nil {
			return nil
		}
		var r Record
		if err := json.Unmarshal(v, &r); err != nil {
			return err
		}
		out = &r
		return nil
	})
	return out, err
}

func (s *Store) GetByHash(hash string) (*Record, error) {
	var out *Record
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(recordsBucket).ForEach(func(_, v []byte) error {
			var r Record
			if err := json.Unmarshal(v, &r); err != nil {
				return err
			}
			if r.SHA256 == hash {
				out = &r
				return errRecordFound
			}
			return nil
		})
	})
	if err == errRecordFound {
		err = nil
	}
	return out, err
}

func (s *Store) Put(r *Record) error {
	return s.Save("", r)
}

func (s *Store) Save(previousSourcePath string, r *Record) error {
	if r == nil {
		return fmt.Errorf("nil record")
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		recB := tx.Bucket(recordsBucket)
		if previousSourcePath != "" && previousSourcePath != r.SourcePath {
			_ = recB.Delete([]byte(previousSourcePath))
		}
		if err := recB.Put([]byte(r.SourcePath), data); err != nil {
			return err
		}
		return nil
	})
}

// GetFailedAIImports returns all records whose AIStatus is "failed".
// It performs a full bucket scan. Records without an AIStatus field
// (legacy records) deserialise to an empty string and are not returned.
func (s *Store) GetFailedAIImports() ([]Record, error) {
	var out []Record
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(recordsBucket).ForEach(func(_, v []byte) error {
			var r Record
			if err := json.Unmarshal(v, &r); err != nil {
				return err
			}
			if r.AIStatus == AIStatusFailed {
				out = append(out, r)
			}
			return nil
		})
	})
	if out == nil {
		out = []Record{}
	}
	return out, err
}

// GetPendingAndFailedAIImports returns work owned by the background worker.
// Legacy records decode as succeeded and therefore are never selected.
func (s *Store) GetPendingAndFailedAIImports() ([]Record, error) {
	var out []Record
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(recordsBucket).ForEach(func(_, v []byte) error {
			var r Record
			if err := json.Unmarshal(v, &r); err != nil {
				return err
			}
			if r.AIStatus == AIStatusPending || r.AIStatus == AIStatusFailed {
				out = append(out, r)
			}
			return nil
		})
	})
	if out == nil {
		out = []Record{}
	}
	return out, err
}
