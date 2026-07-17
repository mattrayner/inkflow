package importer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
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
	return New(cfg, store, provider), &logs
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
