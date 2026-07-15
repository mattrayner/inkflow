## ADDED Requirements

### Requirement: RetryConfig struct exists with required fields

The config package SHALL define a `RetryConfig` struct with:
- `Enabled bool` — whether automatic AI retry is active
- `MaxRetries int` — maximum number of retry attempts (not counting the initial attempt)
- `Backoff string` — duration string for the cooldown between attempts (e.g. `"30s"`)
- `BackoffDuration time.Duration` — parsed form of Backoff, populated during validation

`GeminiConfig` SHALL contain a field `Retry RetryConfig` mapping to the TOML key `retry`.

#### Scenario: Fields map correctly from TOML
- **WHEN** the config file contains `[gemini.retry]` with `enabled = true`, `max_retries = 5`, `backoff = "1m"`
- **THEN** the loaded config SHALL have `Retry.Enabled == true`, `Retry.MaxRetries == 5`, `Retry.Backoff == "1m"`, `Retry.BackoffDuration == 1*time.Minute`

### Requirement: Defaults applied when retry block is absent

When `[gemini.retry]` is not present in the config file, `applyDefaults()` SHALL set:
- `Retry.Enabled = false`
- `Retry.MaxRetries = 3`
- `Retry.Backoff = "30s"`

#### Scenario: Defaults applied on empty gemini block
- **WHEN** no `[gemini]` block or `[gemini.retry]` block is present
- **THEN** the loaded config SHALL have `Retry.Enabled == false`, `Retry.MaxRetries == 3`, `Retry.BackoffDuration == 30*time.Second`

#### Scenario: Defaults applied when only gemini block present
- **WHEN** `[gemini]` is present but `[gemini.retry]` is absent
- **THEN** retry defaults SHALL still be applied

### Requirement: Backoff is validated as a positive Go duration

`validate()` SHALL parse `Retry.Backoff` using `time.ParseDuration`. It SHALL return an error if the value is unparseable, zero, or negative.

#### Scenario: Valid duration accepted
- **WHEN** `backoff = "30s"`
- **THEN** validation SHALL succeed and `BackoffDuration == 30*time.Second`

#### Scenario: Unparseable duration rejected
- **WHEN** `backoff = "banana"`
- **THEN** `Load()` SHALL return a non-nil error containing "backoff"

#### Scenario: Zero duration rejected
- **WHEN** `backoff = "0s"`
- **THEN** `Load()` SHALL return a non-nil error

### Requirement: MaxRetries validated when retry is enabled

When `Retry.Enabled == true`, `validate()` SHALL return an error if `Retry.MaxRetries < 1`.

#### Scenario: MaxRetries zero rejected when enabled
- **WHEN** `enabled = true` and `max_retries = 0`
- **THEN** `Load()` SHALL return a non-nil error containing "max_retries"

#### Scenario: MaxRetries zero accepted when disabled
- **WHEN** `enabled = false` and `max_retries = 0`
- **THEN** `Load()` SHALL succeed (validation is skipped for disabled retry)
