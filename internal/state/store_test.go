package state

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
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
