## ADDED Requirements

### Requirement: OpenAI client implements the ai.Provider contract
The system SHALL provide an `internal/ai/openai` package whose `Client` implements `ai.Provider`, calling the OpenAI Responses API to transcribe a handwritten PDF and produce summary bullets, returning the same `ai.Result{OCR, Summary}` shape the Gemini provider returns.

#### Scenario: Successful OCR + summary
- **WHEN** `Client.Process` is called with a valid PDF reader and the OpenAI API returns a successful structured response containing `ocr_text` and `summary` fields
- **THEN** `Process` returns an `ai.Result` with `OCR` set to the transcription and `Summary` set to the bullet list, and no error

#### Scenario: API error response
- **WHEN** the OpenAI API responds with a non-2xx status
- **THEN** `Process` returns an error containing the API's error message (not the full raw response body when a message field is present), and no partial `ai.Result`

#### Scenario: Malformed structured output
- **WHEN** the OpenAI API returns 2xx but the response body's structured content cannot be parsed into `ocr_text`/`summary`
- **THEN** `Process` returns a descriptive parse error, and the importer's existing "AI failed" marker-block handling applies unchanged

### Requirement: API key is never exposed in transport-level errors
The system SHALL send the OpenAI API key via the `Authorization` request header, never as a URL query parameter, so that no transport error string, log line, or note content can leak the key.

#### Scenario: Request construction
- **WHEN** `Client.Process` builds the outgoing HTTP request
- **THEN** the request URL SHALL NOT contain the API key, and the key SHALL appear only in the `Authorization: Bearer <key>` header

### Requirement: PDF is sent as native file input, not converted to images
The system SHALL submit the PDF to the OpenAI Responses API as inline file data (base64-encoded) alongside the combined OCR+summary prompt text, without a separate page-image conversion step.

#### Scenario: Single PDF submitted
- **WHEN** `Client.Process` receives a PDF via `io.Reader`
- **THEN** the outgoing request body includes one input file part containing the base64-encoded PDF bytes and one text part containing the combined prompt

### Requirement: Structured output schema matches Gemini's contract
The system SHALL request a strict JSON schema response (an object with a string `ocr_text` field and a `summary` array-of-strings field) so response parsing does not depend on free-text heuristics.

#### Scenario: Schema requested
- **WHEN** `Client.Process` builds the request
- **THEN** the request specifies a structured output format requiring `ocr_text` (string, required) and `summary` (array of strings, required)

### Requirement: Model, timeout, and prompts are configurable
The system SHALL allow the OpenAI model name, request timeout, OCR prompt, and summary prompt to be set via `[openai]` config fields, with defaults applied when a field is empty, mirroring the existing `[gemini]` defaulting behavior.

#### Scenario: Defaults applied
- **WHEN** `[openai].model`, `[openai].timeout`, `[openai].ocr_prompt`, or `[openai].summary_prompt` are empty or absent
- **THEN** the system applies documented default values for each empty field, without requiring the user to specify all fields
