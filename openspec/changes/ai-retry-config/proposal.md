## Why

When AI processing fails, there is currently no way to configure automatic retries. The retry behaviour — whether to retry at all, how many times, and how long to wait between attempts — should be user-configurable with sensible defaults. This change adds the config schema, validation, and defaults for that system, so the scheduler (Change 3) has a well-defined contract to read from.

## What Changes

- New `[gemini.retry]` TOML block with three fields: `enabled` (bool), `max_retries` (int), `backoff` (duration string)
- `GeminiConfig` gains a nested `Retry RetryConfig` struct
- `applyDefaults()` sets: `enabled = false`, `max_retries = 3`, `backoff = "30s"`
- `validate()` parses `backoff` as a Go duration, rejects negative or zero values, rejects `max_retries < 1` when enabled
- `inkflow.example.toml` updated with a commented-out `[gemini.retry]` section
- No behaviour change at runtime — config is parsed and validated but not acted on until Change 3

## Capabilities

### New Capabilities
- `ai-retry-config`: Configuration schema and validation for the AI retry system, including enabled flag, maximum retry count, and cooldown backoff duration.

### Modified Capabilities

## Impact

- `internal/config/types.go` — new `RetryConfig` struct, `GeminiConfig.Retry` field
- `internal/config/load.go` — defaults and validation
- `internal/config/load_test.go` — new test cases
- `inkflow.example.toml` — documentation
- No runtime behaviour change; config fields are wired to scheduler in Change 3
