## 1. Config Types

- [x] 1.1 Add `RetryConfig` struct to `internal/config/types.go` with fields: `Enabled bool`, `MaxRetries int`, `Backoff string`, `BackoffDuration time.Duration`
- [x] 1.2 Add `Retry RetryConfig` field to `GeminiConfig` in `internal/config/types.go` with TOML tag `retry`

## 2. Defaults and Validation

- [x] 2.1 In `applyDefaults()` in `internal/config/load.go`, set retry defaults: `Enabled = false`, `MaxRetries = 3`, `Backoff = "30s"` (only when not already set)
- [x] 2.2 In `validate()` in `internal/config/load.go`, parse `Gemini.Retry.Backoff` with `time.ParseDuration`, store result in `BackoffDuration`, and return error if unparseable, zero, or negative
- [x] 2.3 In `validate()`, when `Gemini.Retry.Enabled == true`, return error if `MaxRetries < 1`

## 3. Documentation

- [x] 3.1 Add a commented `[gemini.retry]` section to `inkflow.example.toml` showing all three fields with their defaults and brief inline comments

## 4. Tests

- [x] 4.1 Add `TestLoadParsesRetryConfig` to `internal/config/load_test.go` — asserts all fields parse correctly from a full `[gemini.retry]` block
- [x] 4.2 Add `TestLoadAppliesRetryDefaults` — asserts defaults are applied when `[gemini.retry]` block is absent entirely
- [x] 4.3 Add `TestValidateRetryBackoffRejectedWhenUnparseable` — asserts error on invalid duration string
- [x] 4.4 Add `TestValidateRetryBackoffRejectedWhenZero` — asserts error on `"0s"`
- [x] 4.5 Add `TestValidateMaxRetriesRejectedWhenEnabledAndZero` — asserts error on `max_retries = 0` with `enabled = true`
- [x] 4.6 Add `TestValidateMaxRetriesAcceptedWhenDisabled` — asserts no error on `max_retries = 0` with `enabled = false`
- [x] 4.7 Verify `go test ./internal/config/...` passes
