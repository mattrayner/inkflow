package importer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"inkflow/internal/ai"
	"inkflow/internal/config"
	"inkflow/internal/frontmatter"
	"inkflow/internal/note"
	"inkflow/internal/pdfhash"
	"inkflow/internal/plan"
	"inkflow/internal/state"
)

type Importer struct {
	cfg                  *config.Config
	store                *state.Store
	ai                   ai.Provider
	minReprocessInterval time.Duration
	writeNoteFn          func(plan.Result, string, string) error
	saveRecordFn         func(string, *state.Record) error
}

func New(cfg *config.Config, store *state.Store, aiProvider ai.Provider, minReprocessInterval time.Duration) *Importer {
	return &Importer{cfg: cfg, store: store, ai: aiProvider, minReprocessInterval: minReprocessInterval}
}

func (i *Importer) Import(ctx context.Context, input string, reader io.Reader, modTime time.Time) (*state.Record, error) {
	slog.Default().Debug("import_started", "source", input)
	if !strings.EqualFold(path.Ext(input), ".pdf") {
		return nil, fmt.Errorf("not a pdf: %s", input)
	}

	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	shaSum := sha256.Sum256(data)
	sha := hex.EncodeToString(shaSum[:])
	contentHash := pdfhash.Sum(data)

	t, err := plan.Build(i.cfg.Routes, i.cfg, input, modTime)
	if err != nil {
		return nil, err
	}
	slog.Default().Debug("route_matched", "source", input, "pdf_rel", t.PDFRel, "note_rel", t.NoteRel, "template", t.Template, "ai", t.AI)
	existing, hashMatch, err := i.lookupRecord(input, sha)
	if err != nil {
		return nil, err
	}
	// Dedup: if we already imported the exact same bytes, there's nothing
	// useful to redo — the PDF on disk is identical, the marker blocks
	// already hold the previous AI output, and a re-run would only burn AI
	// tokens and produce visible flicker.
	if existing != nil && existing.SHA256 == sha {
		samePaths := existing.VaultPDFPath == t.PDFRel && existing.VaultNotePath == t.NoteRel
		if samePaths && existing.SourcePath == input && existing.ContentHash != "" {
			slog.Default().Info("dedup_skipped", "source", input, "sha256", sha, "note", t.NoteRel, "pdf", t.PDFRel)
			return existing, nil
		}
		if hashMatch {
			// Content is byte-identical to a previously imported record found
			// only via the hash index (rename and/or a route change moved the
			// output paths). Relocate the existing PDF/note atomically instead
			// of reprocessing.
			return i.relocate(existing, input, modTime, t)
		}
		if samePaths {
			// Backfill legacy records (missing ContentHash) that were found
			// via source path with an exact-hash match and unchanged paths.
			return i.refreshRecord(existing, input, modTime, sha, contentHash, int64(len(data)))
		}
	}
	if existing != nil && existing.VaultPDFPath == t.PDFRel && existing.VaultNotePath == t.NoteRel {
		// Only use the stable fingerprint for the same WebDAV source. A rename
		// can change filename-derived title or tags even when the PDF is
		// unchanged, so it must continue through the normal import path.
		if existing.SourcePath == input && existing.ContentHash != "" && existing.ContentHash == contentHash {
			return i.refreshRecord(existing, input, modTime, sha, contentHash, int64(len(data)))
		}
		// SHA and stable content hash both differ, but if AI previously
		// succeeded recently and debouncing is enabled, this is likely a
		// BOOX wrapper-only rewrite (same handwriting, new export metadata).
		// Refresh the PDF and dedup identity but skip AI and leave the
		// note's marker blocks as-is.
		if t.AI && existing.AIStatus == state.AIStatusSuccess &&
			i.minReprocessInterval > 0 &&
			!existing.AILastSuccessAt.IsZero() &&
			time.Since(existing.AILastSuccessAt) < i.minReprocessInterval {
			return i.refreshWrapper(existing, input, modTime, sha, contentHash, t, data)
		}
	}
	return i.persist(ctx, existing, input, modTime, sha, contentHash, t, data)
}

// refreshWrapper handles a likely BOOX wrapper-only PDF rewrite: full SHA256
// changed but the same route/output paths and a recent AI success mean we
// treat the content as unchanged. It refreshes the on-disk PDF to the newly
// uploaded bytes and advances the dedup identity (SHA/content hash/mod
// time/size), but does NOT call the AI provider and does NOT touch the
// note's marker blocks — the prior AI output stands untouched.
func (i *Importer) refreshWrapper(existing *state.Record, sourcePath string, modTime time.Time, sha, contentHash string, t plan.Result, data []byte) (*state.Record, error) {
	pdfAbs := filepath.Join(i.cfg.VaultDir, filepath.FromSlash(t.PDFRel))
	if err := os.MkdirAll(filepath.Dir(pdfAbs), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(pdfAbs, data, 0o644); err != nil {
		return nil, err
	}
	existing.SHA256 = sha
	existing.ContentHash = contentHash
	existing.SourceModTime = modTime
	existing.SourceSize = int64(len(data))
	existing.ImportedAt = time.Now().UTC()
	// AIStatus/AIRetryCount/AILastError/AILastRetryAt/AILastSuccessAt intentionally
	// left untouched — the previous AI result remains valid and current.
	if err := i.store.Put(existing); err != nil {
		return nil, err
	}
	slog.Default().Info("ai_skipped", "source", sourcePath, "sha256", sha, "reason", "debounced_wrapper_rewrite")
	logImported(sourcePath, t.NoteRel, t.PDFRel)
	return existing, nil
}

// lookupRecord returns whether the record came from a hash match rather than
// the source-path index. Hash matches are not rebound until their import
// outcome has committed, so relocation failures leave state unchanged.
func (i *Importer) lookupRecord(sourcePath, sha string) (*state.Record, bool, error) {
	if old, err := i.store.GetBySourcePath(sourcePath); err != nil {
		return nil, false, err
	} else if old != nil {
		return old, false, nil
	}

	old, err := i.store.GetByHash(sha)
	if err != nil || old == nil {
		return old, false, err
	}
	return old, true, nil
}

// relocate preserves an existing hash-matched PDF/note pair exactly while
// rebinding it to a newly rendered source and vault paths. It copies first and
// removes old paths only after the state update succeeds, so a failed move
// cannot discard the original artifacts.
func (i *Importer) relocate(existing *state.Record, sourcePath string, modTime time.Time, t plan.Result) (*state.Record, error) {
	oldPDF := filepath.Join(i.cfg.VaultDir, filepath.FromSlash(existing.VaultPDFPath))
	oldNote := filepath.Join(i.cfg.VaultDir, filepath.FromSlash(existing.VaultNotePath))
	newPDF := filepath.Join(i.cfg.VaultDir, filepath.FromSlash(t.PDFRel))
	newNote := filepath.Join(i.cfg.VaultDir, filepath.FromSlash(t.NoteRel))

	createdPDF, err := relocateFile(oldPDF, newPDF)
	if err != nil {
		return nil, fmt.Errorf("relocate PDF: %w", err)
	}
	createdNote, err := relocateFile(oldNote, newNote)
	if err != nil {
		if createdPDF {
			_ = os.Remove(newPDF)
		}
		return nil, fmt.Errorf("relocate note: %w", err)
	}

	previousSourcePath := existing.SourcePath
	updated := *existing
	updated.SourcePath = sourcePath
	updated.VaultPDFPath = t.PDFRel
	updated.VaultNotePath = t.NoteRel
	updated.SourceModTime = modTime
	updated.ImportedAt = time.Now().UTC()
	if info, err := os.Stat(newPDF); err != nil {
		return nil, fmt.Errorf("relocate PDF stat: %w", err)
	} else {
		updated.SourceSize = info.Size()
	}
	if err := i.saveRecordOutput(previousSourcePath, &updated); err != nil {
		if createdPDF {
			_ = os.Remove(newPDF)
		}
		if createdNote {
			_ = os.Remove(newNote)
		}
		return nil, err
	}
	if oldPDF != newPDF {
		_ = os.Remove(oldPDF)
	}
	if oldNote != newNote {
		_ = os.Remove(oldNote)
	}
	*existing = updated
	logImported(sourcePath, t.NoteRel, t.PDFRel)
	return existing, nil
}

// relocateFile copies to a non-existent destination. Its boolean result says
// whether this invocation created a destination that may be cleaned up on a
// later failure.
func relocateFile(source, destination string) (bool, error) {
	if source == destination {
		if _, err := os.Stat(source); err != nil {
			return false, err
		}
		return false, nil
	}
	input, err := os.Open(source)
	if err != nil {
		return false, err
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return false, err
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return false, fmt.Errorf("destination already exists: %s", destination)
		}
		return false, err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		_ = os.Remove(destination)
		return false, err
	}
	if err := output.Close(); err != nil {
		_ = os.Remove(destination)
		return false, err
	}
	return true, nil
}

func (i *Importer) refreshRecord(existing *state.Record, sourcePath string, modTime time.Time, sha, contentHash string, size int64) (*state.Record, error) {
	previousSourcePath := existing.SourcePath
	existing.SourcePath = sourcePath
	existing.SHA256 = sha
	existing.ContentHash = contentHash
	existing.SourceModTime = modTime
	existing.SourceSize = size
	existing.ImportedAt = time.Now().UTC()
	if err := i.store.Save(previousSourcePath, existing); err != nil {
		return nil, err
	}
	return existing, nil
}

func (i *Importer) persist(ctx context.Context, existing *state.Record, sourcePath string, modTime time.Time, sha, contentHash string, t plan.Result, pdfData []byte) (*state.Record, error) {
	pdfAbs := filepath.Join(i.cfg.VaultDir, filepath.FromSlash(t.PDFRel))
	noteAbs := filepath.Join(i.cfg.VaultDir, filepath.FromSlash(t.NoteRel))
	if err := os.MkdirAll(filepath.Dir(pdfAbs), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(noteAbs), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(pdfAbs, pdfData, 0o644); err != nil {
		return nil, err
	}
	rec := &state.Record{
		SourcePath:    sourcePath,
		SHA256:        sha,
		ContentHash:   contentHash,
		SourceModTime: modTime,
		SourceSize:    int64(len(pdfData)),
		VaultPDFPath:  t.PDFRel,
		VaultNotePath: t.NoteRel,
		ImportedAt:    time.Now().UTC(),
	}
	previousSourcePath := ""
	previousPDFPath := ""
	previousNotePath := ""
	if existing != nil {
		previousSourcePath = existing.SourcePath
		previousPDFPath = filepath.Join(i.cfg.VaultDir, filepath.FromSlash(existing.VaultPDFPath))
		previousNotePath = filepath.Join(i.cfg.VaultDir, filepath.FromSlash(existing.VaultNotePath))
		// Retry history describes this source record's lifecycle and deliberately
		// survives replacement with the newly imported metadata.
		rec.AIRetryCount = existing.AIRetryCount
		rec.AILastSuccessAt = existing.AILastSuccessAt
		*existing = *rec
		rec = existing
	}

	var summaryBody, ocrBody string
	if t.AI && i.ai != nil {
		slog.Default().Info("ai_called", "source", sourcePath, "sha256", sha)
		res, err := i.ai.Process(ctx, bytes.NewReader(pdfData))
		if err != nil {
			slog.Default().Error("ai_failed", "source", sourcePath, "sha256", sha, "err", err)
			msg := fmt.Sprintf("_AI failed: %s_", err.Error())
			summaryBody, ocrBody = msg, msg
			rec.AIStatus = state.AIStatusFailed
			rec.AIRetryCount++
			rec.AILastError = err.Error()
			rec.AILastRetryAt = time.Now().UTC()
		} else {
			slog.Default().Info("ai_succeeded", "source", sourcePath, "sha256", sha, "ocr_chars", len(res.OCR), "summary_bullets", len(res.Summary))
			if res.OCR != "" {
				ocrBody = res.OCR
			} else {
				ocrBody = "_AI returned no transcription._"
			}
			if len(res.Summary) > 0 {
				summaryBody = "- " + strings.Join(res.Summary, "\n- ")
			} else {
				summaryBody = "_AI returned no summary._"
			}
			rec.AIStatus = state.AIStatusSuccess
			rec.AIRetryCount++
			rec.AILastError = ""
			rec.AILastRetryAt = time.Now().UTC()
			rec.AILastSuccessAt = time.Now().UTC()
		}
	} else if !t.AI {
		slog.Default().Debug("ai_skipped", "source", sourcePath, "reason", "route_ai_disabled")
	} else {
		slog.Default().Debug("ai_skipped", "source", sourcePath, "reason", "no_provider_configured")
	}

	if err := i.writeNoteOutput(t, summaryBody, ocrBody); err != nil {
		removeIfDistinct(previousPDFPath, pdfAbs)
		removeIfDistinct(previousNotePath, noteAbs)
		slog.Default().Error("note_write_failed", "source", sourcePath, "note", t.NoteRel, "err", err)
		return nil, err
	}
	slog.Default().Debug("note_written", "note", noteAbs)
	if err := i.saveRecordOutput(previousSourcePath, rec); err != nil {
		removeIfDistinct(previousPDFPath, pdfAbs)
		removeIfDistinct(previousNotePath, noteAbs)
		slog.Default().Error("state_save_failed", "source", sourcePath, "sha256", sha, "err", err)
		return nil, err
	}
	slog.Default().Debug("state_saved", "source", sourcePath, "sha256", sha)
	if previousPDFPath != "" && previousPDFPath != pdfAbs {
		_ = os.Remove(previousPDFPath)
	}
	if previousNotePath != "" && previousNotePath != noteAbs {
		_ = os.Remove(previousNotePath)
	}
	logImported(sourcePath, t.NoteRel, t.PDFRel)
	return rec, nil
}

func (i *Importer) saveRecord(previousSourcePath string, rec *state.Record) error {
	if previousSourcePath == "" {
		return i.store.Put(rec)
	}
	return i.store.Save(previousSourcePath, rec)
}

func (i *Importer) saveRecordOutput(previousSourcePath string, rec *state.Record) error {
	if i.saveRecordFn != nil {
		return i.saveRecordFn(previousSourcePath, rec)
	}
	return i.saveRecord(previousSourcePath, rec)
}

func (i *Importer) writeNoteOutput(t plan.Result, summaryBody, ocrBody string) error {
	if i.writeNoteFn != nil {
		return i.writeNoteFn(t, summaryBody, ocrBody)
	}
	return i.writeNote(t, summaryBody, ocrBody)
}

func (i *Importer) writeNote(t plan.Result, summaryBody, ocrBody string) error {
	noteAbs := filepath.Join(i.cfg.VaultDir, filepath.FromSlash(t.NoteRel))
	if err := os.MkdirAll(filepath.Dir(noteAbs), 0o755); err != nil {
		return err
	}
	var content string
	if existing, err := os.ReadFile(noteAbs); err == nil {
		content = frontmatter.UpdateTagsWithStrategy(string(existing), t.Tags, t.TagMergeStrategy)
	} else if !os.IsNotExist(err) {
		return err
	} else {
		body, err := plan.RenderNoteBody(i.cfg.TemplateDir, plan.NoteData{
			Date:       t.Date.Format("2006-01-02"),
			Title:      t.Title,
			PDFRelPath: t.PDFRel,
			Template:   t.Template,
			Tags:       t.Tags,
		})
		if err != nil {
			return err
		}
		content = body
	}
	preserveFailure := t.PreserveMarkerOnAIFailure && strings.HasPrefix(summaryBody, "_AI failed:")
	content = note.UpsertMarkerBlockWithFailurePolicy(content, "Summary", "summary", summaryBody, preserveFailure)
	content = note.UpsertMarkerBlockWithFailurePolicy(content, "OCR", "ocr", ocrBody, preserveFailure)
	return os.WriteFile(noteAbs, []byte(content), 0o644)
}

func removeIfDistinct(oldPath, newPath string) {
	if oldPath == "" || oldPath != newPath {
		_ = os.Remove(newPath)
	}
}

func logImported(sourcePath, notePath, pdfPath string) {
	if logger := slog.Default(); logger != nil {
		logger.Info("import_completed", "source", sourcePath, "note", notePath, "pdf", pdfPath)
	}
}

// RetryAI re-runs the AI processing step for an already-imported record.
// It reads the PDF from record.VaultPDFPath, calls the AI provider, and on
// success rewrites the note marker blocks and saves the updated record.
// On any failure it returns an error without modifying the note or the store —
// the caller is responsible for updating the record state.
func (i *Importer) RetryAI(ctx context.Context, rec state.Record) error {
	if i.ai == nil {
		return fmt.Errorf("retry AI: no AI provider configured")
	}

	pdfAbs := filepath.Join(i.cfg.VaultDir, filepath.FromSlash(rec.VaultPDFPath))
	pdfData, err := os.ReadFile(pdfAbs)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("retry AI: vault PDF not found: %s", pdfAbs)
		}
		return fmt.Errorf("retry AI: read vault PDF: %w", err)
	}

	res, err := i.ai.Process(ctx, bytes.NewReader(pdfData))
	if err != nil {
		return err
	}

	var summaryBody, ocrBody string
	if res.OCR != "" {
		ocrBody = res.OCR
	} else {
		ocrBody = "_AI returned no transcription._"
	}
	if len(res.Summary) > 0 {
		summaryBody = "- " + strings.Join(res.Summary, "\n- ")
	} else {
		summaryBody = "_AI returned no summary._"
	}

	noteAbs := filepath.Join(i.cfg.VaultDir, filepath.FromSlash(rec.VaultNotePath))
	existing, err := os.ReadFile(noteAbs)
	if err != nil {
		return fmt.Errorf("retry AI: read vault note: %w", err)
	}
	content := note.UpsertMarkerBlock(string(existing), "Summary", "summary", summaryBody)
	content = note.UpsertMarkerBlock(content, "OCR", "ocr", ocrBody)
	if err := os.WriteFile(noteAbs, []byte(content), 0o644); err != nil {
		return fmt.Errorf("retry AI: write vault note: %w", err)
	}

	rec.AIStatus = state.AIStatusSuccess
	rec.AIRetryCount++
	rec.AILastError = ""
	rec.AILastRetryAt = time.Now().UTC()
	rec.AILastSuccessAt = time.Now().UTC()
	if err := i.store.Put(&rec); err != nil {
		return fmt.Errorf("retry AI: save record: %w", err)
	}
	slog.Info("retry AI: success", "source", rec.SourcePath)
	return nil
}

// WriteNoteError writes an error message into the Summary and OCR marker
// blocks of the note associated with rec. It is called by the retry
// scheduler when retries are exhausted or a non-retryable error occurs.
func (i *Importer) WriteNoteError(rec state.Record, msg string) error {
	noteAbs := filepath.Join(i.cfg.VaultDir, filepath.FromSlash(rec.VaultNotePath))
	existing, err := os.ReadFile(noteAbs)
	if err != nil {
		return fmt.Errorf("write note error: read %s: %w", noteAbs, err)
	}
	preserveFailure := true
	if t, err := plan.Build(i.cfg.Routes, i.cfg, rec.SourcePath, rec.SourceModTime); err == nil {
		preserveFailure = t.PreserveMarkerOnAIFailure
	}
	content := note.UpsertMarkerBlockWithFailurePolicy(string(existing), "Summary", "summary", msg, preserveFailure)
	content = note.UpsertMarkerBlockWithFailurePolicy(content, "OCR", "ocr", msg, preserveFailure)
	return os.WriteFile(noteAbs, []byte(content), 0o644)
}
