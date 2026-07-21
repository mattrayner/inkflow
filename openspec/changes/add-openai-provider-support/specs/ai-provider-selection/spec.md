## ADDED Requirements

### Requirement: Provider is selected via config
The system SHALL determine which AI backend (Gemini or OpenAI) handles PDF OCR/summarization from a single `[ai].provider` config value, defaulting to `"gemini"` when the value is absent or empty.

#### Scenario: No provider configured
- **WHEN** `inkflow.toml` has no `[ai]` table
- **THEN** the system uses the Gemini provider, exactly as it did before this change

#### Scenario: Provider explicitly set to openai
- **WHEN** `inkflow.toml` contains `[ai]\nprovider = "openai"`
- **THEN** the system constructs an OpenAI-backed `ai.Provider` for any route with `ai = true`

#### Scenario: Unknown provider value
- **WHEN** `[ai].provider` is set to a value other than `"gemini"` or `"openai"`
- **THEN** the system SHALL fail startup with an error naming the invalid value and the supported provider names

### Requirement: Startup fails fast when the selected provider lacks credentials
The system SHALL refuse to start if any route has `ai = true` and the selected provider's API key cannot be resolved from its environment variable or configured key file.

#### Scenario: OpenAI selected without a key
- **WHEN** `[ai].provider = "openai"`, at least one route has `ai = true`, and neither `OPENAI_API_KEY` nor `[openai].api_key_file` yields a non-empty key
- **THEN** startup fails with an error identifying OpenAI as the provider and naming the two ways to supply a key

#### Scenario: Gemini selected without a key
- **WHEN** `[ai].provider = "gemini"` (or unset), at least one route has `ai = true`, and neither `GEMINI_API_KEY` nor `[gemini].api_key_file` yields a non-empty key
- **THEN** startup fails exactly as it does today, with an error identifying Gemini and naming the two ways to supply a key

#### Scenario: No route requests AI
- **WHEN** no route has `ai = true`
- **THEN** startup SHALL succeed regardless of whether any provider's API key is configured

### Requirement: Non-selected provider's configuration is ignored
The system SHALL NOT require or validate credentials for a provider that is not currently selected via `[ai].provider`.

#### Scenario: Gemini key present, OpenAI selected, no Gemini validation
- **WHEN** `[ai].provider = "openai"` and `OPENAI_API_KEY` is set, but `GEMINI_API_KEY` is unset and `[gemini].api_key_file` is unset
- **THEN** startup succeeds and no Gemini key resolution error occurs
