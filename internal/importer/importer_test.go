package importer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"inkflow/internal/ai"
	"inkflow/internal/config"
	"inkflow/internal/state"
)

type testAIProvider struct {
	result ai.Result
	err    error
	calls  int
}

func (p *testAIProvider) Process(_ context.Context, pdf io.Reader) (ai.Result, error) {
	p.calls++
	_, _ = io.Copy(io.Discard, pdf)
	return p.result, p.err
}

func newTestImporter(t *testing.T, provider ai.Provider, aiEnabled bool) (*Importer, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := &config.Config{
		VaultDir:       t.TempDir(),
		DefaultPDFDir:  "pdfs",
		DefaultNoteDir: "notes",
		Routes:         []config.Route{{From: "Syncs/", Template: "default", AI: aiEnabled}},
	}
	return New(cfg, store, provider, 0), &logs
}

func importTestPDF(t *testing.T, imp *Importer, contents string) {
	t.Helper()
	if _, err := imp.Import(context.Background(), "Syncs/2026-06-04 note.pdf", strings.NewReader(contents), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

func TestImportLogsAISkippedWhenRouteDisabled(t *testing.T) {
	provider := &testAIProvider{}
	imp, logs := newTestImporter(t, provider, false)

	importTestPDF(t, imp, "pdf-bytes")
	output := logs.String()
	if !strings.Contains(output, "ai_skipped") || !strings.Contains(output, "reason=route_ai_disabled") {
		t.Fatalf("missing disabled-AI log:\n%s", output)
	}
	for _, event := range []string{"ai_called", "ai_succeeded", "ai_failed"} {
		if strings.Contains(output, event) {
			t.Fatalf("unexpected %s log:\n%s", event, output)
		}
	}
	if provider.calls != 0 {
		t.Fatalf("AI calls = %d, want 0", provider.calls)
	}
}

func TestImportLogsAISuccess(t *testing.T) {
	provider := &testAIProvider{result: ai.Result{OCR: "transcript", Summary: []string{"one", "two"}}}
	imp, logs := newTestImporter(t, provider, true)

	importTestPDF(t, imp, "pdf-bytes")
	output := logs.String()
	called := strings.Index(output, "ai_called")
	succeeded := strings.Index(output, "ai_succeeded")
	if called < 0 || succeeded < called || !strings.Contains(output, "import_completed") {
		t.Fatalf("expected successful AI event sequence:\n%s", output)
	}
}

func TestImportLogsAIFailureAndCompletes(t *testing.T) {
	provider := &testAIProvider{err: errors.New("gemini 429: rate limited")}
	imp, logs := newTestImporter(t, provider, true)

	importTestPDF(t, imp, "pdf-bytes")
	output := logs.String()
	called := strings.Index(output, "ai_called")
	failed := strings.Index(output, "ai_failed")
	if called < 0 || failed < called || !strings.Contains(output, "gemini 429: rate limited") || !strings.Contains(output, "import_completed") {
		t.Fatalf("expected failed AI event sequence and completion:\n%s", output)
	}
}

func TestImportLogsDedupSkippedWithoutSecondAICall(t *testing.T) {
	provider := &testAIProvider{result: ai.Result{OCR: "transcript"}}
	imp, logs := newTestImporter(t, provider, true)

	importTestPDF(t, imp, "identical-pdf-bytes")
	importTestPDF(t, imp, "identical-pdf-bytes")
	output := logs.String()
	if !strings.Contains(output, "dedup_skipped") {
		t.Fatalf("missing dedup log:\n%s", output)
	}
	if provider.calls != 1 || strings.Count(output, "ai_called") != 1 {
		t.Fatalf("AI calls = %d, ai_called logs = %d, want 1:\n%s", provider.calls, strings.Count(output, "ai_called"), output)
	}
}

func newDebounceTestImporter(t *testing.T, provider ai.Provider, interval time.Duration) (*Importer, *state.Store, *config.Config, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := &config.Config{
		VaultDir:       t.TempDir(),
		DefaultPDFDir:  "pdfs",
		DefaultNoteDir: "notes",
		Routes:         []config.Route{{From: "Syncs/", Template: "default", AI: true}},
	}
	return New(cfg, store, provider, interval), store, cfg, &logs
}

func importDebouncePDF(t *testing.T, imp *Importer, contents string) *state.Record {
	t.Helper()
	rec, err := imp.Import(context.Background(), "Syncs/2026-06-04 note.pdf", strings.NewReader(contents), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestImportAISuccessRecordsLastSuccessAt(t *testing.T) {
	provider := &testAIProvider{result: ai.Result{OCR: "first ocr", Summary: []string{"first summary"}}}
	imp, store, _, _ := newDebounceTestImporter(t, provider, time.Minute)

	importDebouncePDF(t, imp, "first-pdf")
	stored, err := store.GetBySourcePath("Syncs/2026-06-04 note.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.AILastSuccessAt.IsZero() {
		t.Fatal("AILastSuccessAt is zero after successful AI import")
	}
}

func TestImportDebouncesRecentWrapperRewrite(t *testing.T) {
	provider := &testAIProvider{result: ai.Result{OCR: "first ocr", Summary: []string{"first summary"}}}
	imp, store, cfg, logs := newDebounceTestImporter(t, provider, time.Minute)

	importDebouncePDF(t, imp, "first-pdf")
	notePath := filepath.Join(cfg.VaultDir, "notes", "2026-06-04 note.md")
	pdfPath := filepath.Join(cfg.VaultDir, "pdfs", "2026-06-04-note.pdf")
	noteBefore, err := os.ReadFile(notePath)
	if err != nil {
		t.Fatal(err)
	}

	rec := importDebouncePDF(t, imp, "second-pdf")
	if provider.calls != 1 {
		t.Fatalf("AI calls = %d, want 1", provider.calls)
	}
	if !strings.Contains(logs.String(), "ai_skipped") || !strings.Contains(logs.String(), "reason=debounced_wrapper_rewrite") {
		t.Fatalf("missing debounced wrapper rewrite log:\n%s", logs.String())
	}
	noteAfter, err := os.ReadFile(notePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(noteBefore, noteAfter) {
		t.Fatal("note changed during debounced wrapper rewrite")
	}
	pdfAfter, err := os.ReadFile(pdfPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(pdfAfter) != "second-pdf" {
		t.Errorf("vault PDF = %q, want second upload bytes", pdfAfter)
	}
	if rec.SHA256 != hashString("second-pdf") {
		t.Errorf("returned SHA256 = %q, want %q", rec.SHA256, hashString("second-pdf"))
	}
	stored, err := store.GetBySourcePath("Syncs/2026-06-04 note.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.SHA256 != hashString("second-pdf") {
		t.Fatalf("stored SHA256 = %q, want %q", stored.SHA256, hashString("second-pdf"))
	}
}

func TestImportWrapperRewriteRunsAIWhenDebounceDisabled(t *testing.T) {
	provider := &testAIProvider{result: ai.Result{OCR: "first ocr"}}
	imp, _, cfg, _ := newDebounceTestImporter(t, provider, 0)

	importDebouncePDF(t, imp, "first-pdf")
	provider.result = ai.Result{OCR: "second ocr"}
	importDebouncePDF(t, imp, "second-pdf")
	if provider.calls != 2 {
		t.Fatalf("AI calls = %d, want 2", provider.calls)
	}
	note, err := os.ReadFile(filepath.Join(cfg.VaultDir, "notes", "2026-06-04 note.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(note), "second ocr") || strings.Contains(string(note), "first ocr") {
		t.Fatalf("note was not rewritten with current AI output:\n%s", note)
	}
}

func TestImportDoesNotDebounceFailedAI(t *testing.T) {
	provider := &testAIProvider{err: errors.New("temporary AI failure")}
	imp, _, _, _ := newDebounceTestImporter(t, provider, time.Minute)

	importDebouncePDF(t, imp, "first-pdf")
	provider.err = nil
	provider.result = ai.Result{OCR: "second ocr"}
	importDebouncePDF(t, imp, "second-pdf")
	if provider.calls != 2 {
		t.Fatalf("AI calls = %d, want 2 after failed first attempt", provider.calls)
	}
}

func TestImportDoesNotDebounceRouteOutputPathChange(t *testing.T) {
	provider := &testAIProvider{result: ai.Result{OCR: "first ocr"}}
	imp, _, cfg, _ := newDebounceTestImporter(t, provider, time.Minute)

	importDebouncePDF(t, imp, "first-pdf")
	cfg.DefaultPDFDir = "moved-pdfs"
	cfg.DefaultNoteDir = "moved-notes"
	provider.result = ai.Result{OCR: "second ocr"}
	importDebouncePDF(t, imp, "second-pdf")
	if provider.calls != 2 {
		t.Fatalf("AI calls = %d, want 2 after route output path change", provider.calls)
	}
	if _, err := os.Stat(filepath.Join(cfg.VaultDir, "moved-pdfs", "2026-06-04-note.pdf")); err != nil {
		t.Fatalf("moved PDF missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.VaultDir, "moved-notes", "2026-06-04 note.md")); err != nil {
		t.Fatalf("moved note missing: %v", err)
	}
}
