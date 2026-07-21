package state

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestGetFailedAIImports_EmptyWhenNone(t *testing.T) {
	s := openTestStore(t)
	got, err := s.GetFailedAIImports()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty slice, got %d record(s)", len(got))
	}
}

func TestGetFailedAIImports_ReturnsOnlyFailedRecords(t *testing.T) {
	s := openTestStore(t)

	now := time.Now().UTC()

	failedRec := &Record{
		SourcePath:    "/sync/bad.pdf",
		SHA256:        "abc123",
		VaultPDFPath:  "pdfs/bad.pdf",
		VaultNotePath: "notes/bad.md",
		ImportedAt:    now,
		AIStatus:      AIStatusFailed,
		AIRetryCount:  1,
		AILastError:   "gemini 503: unavailable",
		AILastRetryAt: now,
	}
	successRec := &Record{
		SourcePath:    "/sync/good.pdf",
		SHA256:        "def456",
		VaultPDFPath:  "pdfs/good.pdf",
		VaultNotePath: "notes/good.md",
		ImportedAt:    now,
		AIStatus:      AIStatusSuccess,
		AIRetryCount:  1,
		AILastRetryAt: now,
	}
	noAIRec := &Record{
		SourcePath:    "/sync/noai.pdf",
		SHA256:        "ghi789",
		VaultPDFPath:  "pdfs/noai.pdf",
		VaultNotePath: "notes/noai.md",
		ImportedAt:    now,
		// AIStatus intentionally empty — route had no AI
	}

	for _, r := range []*Record{failedRec, successRec, noAIRec} {
		if err := s.Put(r); err != nil {
			t.Fatalf("Put(%s): %v", r.SourcePath, err)
		}
	}

	got, err := s.GetFailedAIImports()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 failed record, got %d", len(got))
	}
	if got[0].SourcePath != failedRec.SourcePath {
		t.Errorf("expected source path %q, got %q", failedRec.SourcePath, got[0].SourcePath)
	}
	if got[0].AILastError != failedRec.AILastError {
		t.Errorf("expected AILastError %q, got %q", failedRec.AILastError, got[0].AILastError)
	}
}

func TestGetFailedAIImports_OldZeroValueRecordsNotReturned(t *testing.T) {
	s := openTestStore(t)

	// Simulate a legacy record that has no AI fields (they'll deserialise as zero values).
	legacy := &Record{
		SourcePath:    "/sync/legacy.pdf",
		SHA256:        "legacy001",
		VaultPDFPath:  "pdfs/legacy.pdf",
		VaultNotePath: "notes/legacy.md",
		ImportedAt:    time.Now().UTC(),
		// AIStatus == "" — not attempted, no AI configured
	}
	if err := s.Put(legacy); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := s.GetFailedAIImports()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("legacy record with empty AIStatus must not appear in failed imports, got %d record(s)", len(got))
	}
}

func TestLegacyRecordDefaultsToSucceeded(t *testing.T) {
	var got Record
	if err := json.Unmarshal([]byte(`{"source_path":"/sync/legacy.pdf","sha256":"legacy001"}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.AIStatus != AIStatusSuccess {
		t.Fatalf("legacy status = %+v, want succeeded", got)
	}
}

func TestGetFailedAIImports_ReturnsMultipleFailed(t *testing.T) {
	s := openTestStore(t)

	now := time.Now().UTC()
	for i, path := range []string{"/sync/a.pdf", "/sync/b.pdf", "/sync/c.pdf"} {
		r := &Record{
			SourcePath:    path,
			SHA256:        string(rune('a' + i)),
			VaultPDFPath:  "pdfs/x.pdf",
			VaultNotePath: "notes/x.md",
			ImportedAt:    now,
			AIStatus:      AIStatusFailed,
			AIRetryCount:  1,
			AILastError:   "some error",
			AILastRetryAt: now,
		}
		if err := s.Put(r); err != nil {
			t.Fatalf("Put(%s): %v", path, err)
		}
	}

	got, err := s.GetFailedAIImports()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("expected 3 failed records, got %d", len(got))
	}
}

func TestSaveAndDeleteMaintainIndexes(t *testing.T) {
	s := openTestStore(t)

	old := &Record{SourcePath: "/sync/old.pdf", SHA256: "old-hash", AIStatus: AIStatusFailed}
	if err := s.Put(old); err != nil {
		t.Fatalf("Put(old): %v", err)
	}
	replacement := &Record{SourcePath: "/sync/new.pdf", SHA256: "new-hash", AIStatus: AIStatusSuccess}
	if err := s.Save(old.SourcePath, replacement); err != nil {
		t.Fatalf("Save(replacement): %v", err)
	}
	assertIndexes(t, s, map[string][]string{"new-hash": {"/sync/new.pdf"}}, nil)

	if err := s.Delete(replacement.SourcePath); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	assertIndexes(t, s, map[string][]string{}, nil)
	if got, err := s.GetBySourcePath(replacement.SourcePath); err != nil || got != nil {
		t.Fatalf("GetBySourcePath after Delete = %#v, %v; want nil, nil", got, err)
	}
}

func TestIndexedLookups(t *testing.T) {
	s := openTestStore(t)
	first := &Record{SourcePath: "/sync/a.pdf", SHA256: "shared", AIStatus: AIStatusFailed}
	second := &Record{SourcePath: "/sync/b.pdf", SHA256: "shared", AIStatus: AIStatusSuccess}
	if err := s.Put(first); err != nil {
		t.Fatalf("Put(first): %v", err)
	}
	if err := s.Put(second); err != nil {
		t.Fatalf("Put(second): %v", err)
	}
	assertIndexes(t, s, map[string][]string{"shared": {first.SourcePath, second.SourcePath}}, []string{first.SourcePath})

	got, err := s.GetByHash("shared")
	if err != nil || got == nil || got.SourcePath != first.SourcePath {
		t.Fatalf("GetByHash(shared) = %#v, %v; want %q", got, err, first.SourcePath)
	}
	failed, err := s.GetFailedAIImports()
	if err != nil || len(failed) != 1 || failed[0].SourcePath != first.SourcePath {
		t.Fatalf("GetFailedAIImports = %#v, %v; want %q", failed, err, first.SourcePath)
	}
}

func TestOpenBackfillsLegacyIndexes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	legacy := &Record{SourcePath: "/sync/legacy.pdf", SHA256: "legacy-hash", AIStatus: AIStatusFailed}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("Marshal legacy record: %v", err)
	}
	db, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatalf("Open fixture DB: %v", err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		bucket, err := tx.CreateBucket(recordsBucket)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(legacy.SourcePath), data)
	}); err != nil {
		_ = db.Close()
		t.Fatalf("Write fixture DB: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close fixture DB: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open legacy DB: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	assertIndexes(t, s, map[string][]string{legacy.SHA256: {legacy.SourcePath}}, []string{legacy.SourcePath})
	if got, err := s.GetByHash(legacy.SHA256); err != nil || got == nil || got.SourcePath != legacy.SourcePath {
		t.Fatalf("GetByHash after backfill = %#v, %v", got, err)
	}
}

func TestDeadPropertiesPersistAndApplyAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	changes := []DeadPropertyChange{{DeadProperty: DeadProperty{Namespace: "urn:test", Local: "one", Value: "value"}}}
	if err := s.ApplyDeadPropertyChanges("note.md", changes); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	properties, err := s.GetDeadProperties("note.md")
	if err != nil || len(properties) != 1 || properties[0].Value != "value" {
		t.Fatalf("persisted properties = %#v, %v", properties, err)
	}
	if err := s.ApplyDeadPropertyChanges("note.md", []DeadPropertyChange{{DeadProperty: properties[0], Remove: true}}); err != nil {
		t.Fatal(err)
	}
	properties, err = s.GetDeadProperties("note.md")
	if err != nil || len(properties) != 0 {
		t.Fatalf("removed properties = %#v, %v", properties, err)
	}
}

func assertIndexes(t *testing.T, s *Store, hashes map[string][]string, failed []string) {
	t.Helper()
	if err := s.db.View(func(tx *bbolt.Tx) error {
		hashB := tx.Bucket(hashIndexBucket)
		failedB := tx.Bucket(failedIndexBucket)
		if hashB == nil || failedB == nil {
			t.Fatal("index buckets must exist")
		}
		for hash, paths := range hashes {
			pathsB := hashB.Bucket(hashIndexKey(hash))
			if pathsB == nil {
				t.Fatalf("missing hash index for %q", hash)
			}
			for _, path := range paths {
				if got := pathsB.Get([]byte(path)); string(got) != path {
					t.Fatalf("hash index %q[%q] = %q", hash, path, got)
				}
			}
			if pathsB.Stats().KeyN != len(paths) {
				t.Fatalf("hash index %q has %d entries; want %d", hash, pathsB.Stats().KeyN, len(paths))
			}
		}
		bucketCount := 0
		if err := hashB.ForEach(func(_, v []byte) error {
			if v == nil {
				bucketCount++
			}
			return nil
		}); err != nil {
			return err
		}
		if bucketCount != len(hashes) {
			t.Fatalf("hash index has %d buckets; want %d", bucketCount, len(hashes))
		}
		for _, path := range failed {
			if got := failedB.Get([]byte(path)); string(got) != path {
				t.Fatalf("failed index[%q] = %q", path, got)
			}
		}
		if failedB.Stats().KeyN != len(failed) {
			t.Fatalf("failed index has %d entries; want %d", failedB.Stats().KeyN, len(failed))
		}
		return nil
	}); err != nil {
		t.Fatalf("inspect indexes: %v", err)
	}
}
