## Context

Inkflow's TOML config is parsed in `internal/config/` with a clear pattern: `types.go` defines structs, `load.go` handles `applyDefaults()` and `validate()`, and tests in `load_test.go` cover each path. The `[gemini]` block already has a nested pattern (`APIKeyFile`, `Model`, `Timeout`) and the new `[gemini.retry]` block follows the same shape.

The retry system is opt-in (`enabled = false` by default) so existing users are unaffected. When enabled, it needs just two additional parameters: how many times to retry (`max_retries`) and how long to wait between each attempt (`backoff`).

## Goals / Non-Goals

**Goals:**
- Define `RetryConfig` struct with `Enabled bool`, `MaxRetries int`, `Backoff string`
- Nest it as `Retry RetryConfig` inside `GeminiConfig`
- Apply defaults when `[gemini.retry]` block is absent
- Validate: backoff parses as a valid positive Go duration; max_retries >= 1 when enabled
- Add example TOML and test coverage

**Non-Goals:**
- Per-route retry overrides (global config only; per-route is out of scope)
- Retryable error classification (belongs in the scheduler or Gemini client, Change 3)
- Any runtime wiring — this change only parses and validates

## Decisions

### Single global retry config under `[gemini.retry]`, not per-route

**Decision:** One `[gemini.retry]` block applies to all AI-enabled routes.

**Rationale:** Per-route overrides add significant config surface and TOML verbosity for minimal practical benefit. All routes share the same Gemini client and the same failure characteristics. Global config is simpler to validate, document, and reason about.

**Alternative considered:** `[routes.*.retry]` per-route block. Rejected: disproportionate complexity.

### `backoff` stored as a string, parsed at validation time

**Decision:** `Backoff string` in the struct, parsed via `time.ParseDuration()` in `validate()`.

**Rationale:** Consistent with `Timeout string` in `GeminiConfig`. Users write human-readable durations (`"30s"`, `"2m"`). The parsed `time.Duration` is stored on `RetryConfig` as a separate `BackoffDuration time.Duration` field (unexported or exported — TBD by implementer) to avoid re-parsing at runtime.

### Default `enabled = false`

**Decision:** Retry is off by default.

**Rationale:** Existing deployments should not silently start retrying — that could cause unexpected Gemini API usage or duplicate note content. Opt-in is the safe default.

### Default `max_retries = 3`, `backoff = "30s"`

**Rationale:** Three attempts covers transient Gemini 429/503 errors with a 30-second cooldown. Total worst-case delay for one import is ~90 seconds, which is acceptable given BOOX uploads are infrequent and user-initiated.

## Risks / Trade-offs

- **Risk: User sets very short backoff (e.g., `"1s"`)** → Risk of hammering Gemini API during outage. Mitigation: document recommended minimum; no enforced lower bound (we trust the user).
- **Risk: `enabled = false` default surprises users who added the block** → Clear documentation in example TOML.

## Migration Plan

No migration required. New config fields with defaults; existing `inkflow.toml` files without `[gemini.retry]` will use the defaults silently.

## Open Questions

- Should `BackoffDuration` be exported on the struct (for use by the scheduler in Change 3)? → Yes, export it as `BackoffDuration time.Duration` for clean cross-package access.
