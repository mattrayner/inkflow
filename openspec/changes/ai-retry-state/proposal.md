## Why

When AI transcription or summarisation fails, the error is written into the Obsidian note but there is no persistent record of whether an import's AI processing succeeded or failed. This makes it impossible to programmatically identify imports that need re-processing, and it couples failure visibility entirely to the content of the note file rather than the application's own state.

## What Changes

- `state.Record` gains four new fields: `AIStatus` (enum string), `AIRetryCount` (int), `AILastError` (string), `AILastRetryAt` (time.Time)
- `importer.persist()` updates the record's AI status to `success` or `failed` after every AI call, and stores the error message on failure
- On successful AI processing, the marker blocks are written as today (no behavioural change)
- On failed AI processing, the record is marked `failed` and the error message is stored; the existing error-text-in-note behaviour is preserved for now
- A new `state.Store` query method `GetFailedAIImports()` returns all records with `AIStatus == "failed"`
- Existing records in BoltDB without the new fields deserialise cleanly (Go JSON zero-values: empty string, zero int, zero time)

## Capabilities

### New Capabilities
- `ai-import-status`: Persistent tracking of per-import AI processing outcome (success, failed, or not yet attempted) stored in the state DB alongside the existing import record.

### Modified Capabilities

## Impact

- `internal/state/store.go` — Record struct, new query method
- `internal/importer/importer.go` — persist() updates AI status after every AI call
- `internal/state/` tests — new coverage for GetFailedAIImports
- No config changes, no new dependencies, no breaking changes to existing behaviour
