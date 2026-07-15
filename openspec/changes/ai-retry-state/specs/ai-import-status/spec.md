## ADDED Requirements

### Requirement: AI status is persisted per import record

The state store Record SHALL include fields to track the outcome of AI processing for each import:
- `AIStatus string` — one of `""` (no AI attempted), `"success"`, `"failed"`
- `AIRetryCount int` — number of AI processing attempts made (0 if none)
- `AILastError string` — error message from the most recent failed attempt (empty if success or not attempted)
- `AILastRetryAt time.Time` — UTC timestamp of the most recent AI attempt (zero if none)

#### Scenario: Record for non-AI route has empty AI status
- **WHEN** an import completes on a route with `ai = false`
- **THEN** the stored record SHALL have `AIStatus == ""`
- **AND** `AIRetryCount == 0`
- **AND** `AILastError == ""`

#### Scenario: Record updated to success after successful AI call
- **WHEN** `ai.Process()` returns without error
- **THEN** the importer SHALL update the record with `AIStatus = "success"`
- **AND** `AIRetryCount` SHALL be incremented by 1
- **AND** `AILastError` SHALL be set to `""`
- **AND** `AILastRetryAt` SHALL be set to the current UTC time

#### Scenario: Record updated to failed after AI error
- **WHEN** `ai.Process()` returns an error
- **THEN** the importer SHALL update the record with `AIStatus = "failed"`
- **AND** `AIRetryCount` SHALL be incremented by 1
- **AND** `AILastError` SHALL be set to the error message string
- **AND** `AILastRetryAt` SHALL be set to the current UTC time

#### Scenario: Existing records without AI status fields load cleanly
- **WHEN** a BoltDB record written before this change is deserialised
- **THEN** `AIStatus` SHALL be `""`, `AIRetryCount` SHALL be `0`, `AILastError` SHALL be `""`, `AILastRetryAt` SHALL be the zero `time.Time`
- **AND** the import SHALL not be treated as failed or pending

### Requirement: Store exposes query for failed AI imports

The `state.Store` SHALL provide a method `GetFailedAIImports() ([]Record, error)` that returns all records where `AIStatus == "failed"`.

#### Scenario: Returns only failed records
- **WHEN** the store contains records with mixed AIStatus values
- **THEN** `GetFailedAIImports()` SHALL return only those with `AIStatus == "failed"`

#### Scenario: Returns empty slice when no failures exist
- **WHEN** no records have `AIStatus == "failed"`
- **THEN** `GetFailedAIImports()` SHALL return an empty (non-nil) slice and nil error
