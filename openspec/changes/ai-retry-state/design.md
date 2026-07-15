## Context

Inkflow stores import records in BoltDB via `internal/state/store.go`. The `Record` struct captures file metadata and vault paths but has no field representing whether AI processing (OCR + summary via Gemini) succeeded or failed. When AI fails, an error string is written into the Obsidian note's marker blocks, but the application's own state is silent on the matter.

This means there is no programmatic way to find imports with failed AI — you would have to scan every note file for `_AI failed:` text. This is a prerequisite for any retry system: we must know which records need retrying before we can retry them.

BoltDB serialises records as JSON. Go's `encoding/json` zero-values missing fields on deserialisation, so adding new fields to `Record` is backward-compatible without any explicit migration step.

## Goals / Non-Goals

**Goals:**
- Add four fields to `state.Record`: `AIStatus`, `AIRetryCount`, `AILastError`, `AILastRetryAt`
- Define a string enum for `AIStatus`: `""` (not attempted / no AI route), `"success"`, `"failed"`
- Update `importer.persist()` to write AI status into the record after every AI call
- Add `state.Store.GetFailedAIImports()` to query all records with `AIStatus == "failed"`
- Preserve existing note-writing behaviour (error text still written to marker blocks)

**Non-Goals:**
- Retry logic (Change 3: ai-retry-scheduler)
- Config changes (Change 2: ai-retry-config)
- Any change to the Gemini client or Provider interface

## Decisions

### Store AI status in the existing Record, not a separate bucket

**Decision:** Add fields directly to `state.Record` rather than creating a second BoltDB bucket.

**Rationale:** Records are already keyed by source path and SHA256. A separate bucket would require joins and two round-trips. The existing JSON serialisation handles new fields transparently (zero-value on old records). This keeps the query model simple.

**Alternative considered:** Separate `ai_records` bucket with its own key. Rejected: extra complexity, two writes per import, no clear benefit at this scale.

### AIStatus as a string enum, not bool

**Decision:** `AIStatus string` with values `""`, `"success"`, `"failed"` rather than a bool `AISucceeded`.

**Rationale:** Three states are needed: not-attempted (route has no AI), success, failed. A bool can't represent not-attempted cleanly. A string also makes BoltDB values human-readable during debugging.

### AIRetryCount tracks cumulative attempts, not remaining

**Decision:** `AIRetryCount int` starts at 0 and increments on each attempt (first attempt sets it to 1).

**Rationale:** The scheduler (Change 3) compares `AIRetryCount` against the configured `MaxRetries`. Counting up is unambiguous and survives config changes — if `MaxRetries` is lowered after the fact, records with `AIRetryCount >= MaxRetries` are simply not retried.

## Risks / Trade-offs

- **Risk: Old records have `AIStatus == ""`** → Scheduler (Change 3) must treat `""` as "not applicable" not "pending". This is documented in the scheduler spec.
- **Risk: Concurrent PUT for the same file** → BoltDB serialises writes; the second PUT will overwrite the record. This is existing behaviour and not worsened.
- **Risk: persist() partial failure** (PDF written, note write fails before record update) → Existing risk, not introduced here. The record update is last in persist(), consistent with current ordering.

## Migration Plan

No migration required. Existing BoltDB records deserialise with `AIStatus == ""`, `AIRetryCount == 0`, zero `AILastRetryAt`, empty `AILastError`. The scheduler change will treat `""` status as "no AI was configured" and ignore those records.

## Open Questions

- None — this change is self-contained and unambiguous.
