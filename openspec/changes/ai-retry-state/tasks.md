## 1. State Record Schema

- [x] 1.1 Add `AIStatus string`, `AIRetryCount int`, `AILastError string`, `AILastRetryAt time.Time` fields to `state.Record` in `internal/state/store.go`
- [x] 1.2 Add `GetFailedAIImports() ([]Record, error)` method to `state.Store` — full bucket scan returning records where `AIStatus == "failed"`

## 2. Importer Integration

- [x] 2.1 In `importer.persist()`, after a successful `ai.Process()` call, update the record: set `AIStatus = "success"`, increment `AIRetryCount`, clear `AILastError`, set `AILastRetryAt = time.Now().UTC()`, and call `store.Save(record)`
- [x] 2.2 In `importer.persist()`, after a failed `ai.Process()` call, update the record: set `AIStatus = "failed"`, increment `AIRetryCount`, set `AILastError` to the error message, set `AILastRetryAt = time.Now().UTC()`, and call `store.Save(record)` — existing error-text-in-note behaviour is unchanged

## 3. Tests

- [x] 3.1 Add unit tests in `internal/state/` for `GetFailedAIImports()`: returns only failed records, returns empty slice when none exist, old zero-value records are not returned
- [x] 3.2 Add or extend tests in `internal/webdavserver/server_test.go` to assert that after a successful AI call the stored record has `AIStatus == "success"` and `AIRetryCount == 1`
- [x] 3.3 Add or extend tests in `internal/webdavserver/server_test.go` to assert that after a failed AI call the stored record has `AIStatus == "failed"`, `AIRetryCount == 1`, and `AILastError` is non-empty
- [x] 3.4 Verify `go test ./...` passes with no race conditions (`go test -race ./...`)
