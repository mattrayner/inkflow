package state

import (
	"encoding/base64"
	"encoding/json"
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
	recordsBucket        = []byte("records")
	hashIndexBucket      = []byte("hash_index")
	failedIndexBucket    = []byte("failed_index")
	deadPropertiesBucket = []byte("dead_properties")
)

type Store struct {
	db *bbolt.DB
}

// DeadProperty is a WebDAV property stored independently from an imported
// resource record. Value contains the property's XML inner content.
type DeadProperty struct {
	Namespace string `json:"namespace"`
	Local     string `json:"local"`
	Value     string `json:"value"`
}

// DeadPropertyChange describes one ordered PROPPATCH set or remove operation.
type DeadPropertyChange struct {
	DeadProperty
	Remove bool
}

type Record struct {
	SourcePath    string    `json:"source_path"`
	SHA256        string    `json:"sha256"`
	ContentHash   string    `json:"content_hash,omitempty"`
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
		hashIndexMissing := tx.Bucket(hashIndexBucket) == nil
		failedIndexMissing := tx.Bucket(failedIndexBucket) == nil
		if _, err := tx.CreateBucketIfNotExists(hashIndexBucket); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(failedIndexBucket); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(deadPropertiesBucket); err != nil {
			return err
		}
		if !hashIndexMissing && !failedIndexMissing {
			return nil
		}
		return tx.Bucket(recordsBucket).ForEach(func(_, v []byte) error {
			var r Record
			if err := json.Unmarshal(v, &r); err != nil {
				return err
			}
			return addRecordIndexes(tx, &r)
		})
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
		hashPaths := tx.Bucket(hashIndexBucket).Bucket(hashIndexKey(hash))
		if hashPaths == nil {
			return nil
		}
		_, sourcePath := hashPaths.Cursor().First()
		if sourcePath == nil {
			return nil
		}
		v := tx.Bucket(recordsBucket).Get(sourcePath)
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

// GetByVaultPDFPath returns the import record whose PDF output is rel. This
// permits retrieval to reuse the importer-computed SHA256 as a strong ETag.
func (s *Store) GetByVaultPDFPath(rel string) (*Record, error) {
	var out *Record
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(recordsBucket).ForEach(func(_, v []byte) error {
			var r Record
			if err := json.Unmarshal(v, &r); err != nil {
				return err
			}
			if r.VaultPDFPath == rel {
				copy := r
				out = &copy
			}
			return nil
		})
	})
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
			if err := removeRecordIndexesForSourcePath(tx, previousSourcePath); err != nil {
				return err
			}
			if err := recB.Delete([]byte(previousSourcePath)); err != nil {
				return err
			}
		}
		if err := removeRecordIndexesForSourcePath(tx, r.SourcePath); err != nil {
			return err
		}
		if err := recB.Put([]byte(r.SourcePath), data); err != nil {
			return err
		}
		return addRecordIndexes(tx, r)
	})
}

// Delete removes a record and its derived index entries atomically.
func (s *Store) Delete(sourcePath string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		recB := tx.Bucket(recordsBucket)
		if recB.Get([]byte(sourcePath)) == nil {
			return nil
		}
		if err := removeRecordIndexesForSourcePath(tx, sourcePath); err != nil {
			return err
		}
		return recB.Delete([]byte(sourcePath))
	})
}

// GetDeadProperties returns the persisted custom properties for a vault-relative
// resource path.
func (s *Store) GetDeadProperties(resourcePath string) ([]DeadProperty, error) {
	var properties []DeadProperty
	err := s.db.View(func(tx *bbolt.Tx) error {
		value := tx.Bucket(deadPropertiesBucket).Get([]byte(resourcePath))
		if value == nil {
			return nil
		}
		return json.Unmarshal(value, &properties)
	})
	if properties == nil {
		properties = []DeadProperty{}
	}
	return properties, err
}

// ApplyDeadPropertyChanges applies every change in one Bolt transaction. A
// caller first validates an entire PROPPATCH request, so a failed request never
// invokes this method and cannot leave partial property writes behind.
func (s *Store) ApplyDeadPropertyChanges(resourcePath string, changes []DeadPropertyChange) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(deadPropertiesBucket)
		var properties []DeadProperty
		if value := bucket.Get([]byte(resourcePath)); value != nil {
			if err := json.Unmarshal(value, &properties); err != nil {
				return err
			}
		}
		byName := make(map[string]DeadProperty, len(properties))
		for _, property := range properties {
			byName[property.Namespace+"\x00"+property.Local] = property
		}
		for _, change := range changes {
			key := change.Namespace + "\x00" + change.Local
			if change.Remove {
				delete(byName, key)
				continue
			}
			byName[key] = change.DeadProperty
		}
		properties = properties[:0]
		for _, property := range byName {
			properties = append(properties, property)
		}
		if len(properties) == 0 {
			return bucket.Delete([]byte(resourcePath))
		}
		data, err := json.Marshal(properties)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(resourcePath), data)
	})
}

// GetFailedAIImports returns all records whose AIStatus is "failed".
// Records without an AIStatus field (legacy records) are not returned.
func (s *Store) GetFailedAIImports() ([]Record, error) {
	var out []Record
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(failedIndexBucket).ForEach(func(_, sourcePath []byte) error {
			v := tx.Bucket(recordsBucket).Get(sourcePath)
			if v == nil {
				return nil
			}
			var r Record
			if err := json.Unmarshal(v, &r); err != nil {
				return err
			}
			out = append(out, r)
			return nil
		})
	})
	if out == nil {
		out = []Record{}
	}
	return out, err
}

func hashIndexKey(hash string) []byte {
	return []byte("hash:" + base64.RawURLEncoding.EncodeToString([]byte(hash)))
}

func addRecordIndexes(tx *bbolt.Tx, r *Record) error {
	hashPaths, err := tx.Bucket(hashIndexBucket).CreateBucketIfNotExists(hashIndexKey(r.SHA256))
	if err != nil {
		return err
	}
	sourcePath := []byte(r.SourcePath)
	if err := hashPaths.Put(sourcePath, sourcePath); err != nil {
		return err
	}
	if r.AIStatus == AIStatusFailed {
		return tx.Bucket(failedIndexBucket).Put(sourcePath, sourcePath)
	}
	return nil
}

func removeRecordIndexesForSourcePath(tx *bbolt.Tx, sourcePath string) error {
	recB := tx.Bucket(recordsBucket)
	v := recB.Get([]byte(sourcePath))
	if v == nil {
		return nil
	}
	var r Record
	if err := json.Unmarshal(v, &r); err != nil {
		return err
	}
	hashB := tx.Bucket(hashIndexBucket)
	hashKey := hashIndexKey(r.SHA256)
	if hashPaths := hashB.Bucket(hashKey); hashPaths != nil {
		if err := hashPaths.Delete([]byte(sourcePath)); err != nil {
			return err
		}
		firstKey, _ := hashPaths.Cursor().First()
		if firstKey == nil {
			if err := hashB.DeleteBucket(hashKey); err != nil {
				return err
			}
		}
	}
	if r.AIStatus == AIStatusFailed {
		if err := tx.Bucket(failedIndexBucket).Delete([]byte(sourcePath)); err != nil {
			return err
		}
	}
	return nil
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
