## 1. Config: generalize provider selection

- [x] 1.1 Add `AI AIConfig` to `internal/config/types.go` with `AIConfig{Provider string}` (`toml:"ai"`), and add `OpenAI OpenAIConfig` field with `APIKeyFile, Model, Timeout, OCRPrompt, SummaryPrompt` (mirroring `GeminiConfig`).
- [x] 1.2 In `internal/config/load.go` `applyDefaults`: default `cfg.AI.Provider` to `"gemini"` when empty; default `cfg.OpenAI.Model` to `"gpt-4.1"`, `cfg.OpenAI.Timeout` to `"60s"`, and set OCR/summary prompt defaults for OpenAI (can reuse the same prompt text as Gemini's defaults).
- [x] 1.3 In `internal/config/load.go` `validate`: reject unknown `cfg.AI.Provider` values (only `"gemini"`/`"openai"` allowed) with a clear error message.
- [x] 1.4 Update `inkflow.example.toml` with a commented `[ai]` + `[openai]` example section alongside the existing `[gemini]` section.
- [x] 1.5 Add/extend `internal/config/load_test.go` cases: default provider is gemini when `[ai]` absent, explicit `provider = "openai"` parses, invalid provider value fails validation, OpenAI defaults applied when `[openai]` fields empty.

## 2. OpenAI provider implementation

- [x] 2.1 Create `internal/ai/openai/config.go` with `ClientConfig{Endpoint, APIKey, Model, Timeout, OCRPrompt, SummaryPrompt}` mirroring `gemini.ClientConfig`.
- [x] 2.2 Create `internal/ai/openai/client.go`: `New(cfg ClientConfig) *Client`, compile-time `var _ ai.Provider = (*Client)(nil)`, `defaultEndpoint = "https://api.openai.com"`.
- [x] 2.3 Implement `Client.Process`: build a `POST {endpoint}/v1/responses` request with an `input` array containing one message with a text part (combined OCR+summary prompt) and an `input_file` part carrying `filename` + base64 `file_data`; set `text.format` to a strict JSON schema requiring `ocr_text` (string) and `summary` (array of strings), matching Gemini's `response_schema` semantics.
- [x] 2.4 Set `Authorization: Bearer <APIKey>` header; do not put the key in the URL or query string.
- [x] 2.5 Parse the Responses API output into `ai.Result`; handle non-2xx status by extracting the API's `error.message` field (fallback to trimmed raw body), matching `gemini/client.go`'s `extractErrorMessage` pattern.
- [x] 2.6 Handle any observed double-escaped-newline or similar JSON-mode quirks in OpenAI's structured output the same way `gemini/client.go`'s `unescapeNewlines` does, if applicable after live testing; otherwise document why it's unnecessary.
- [x] 2.7 Write `internal/ai/openai/client_test.go` mirroring `gemini/client_test.go`: successful parse, non-2xx error surfacing, malformed JSON payload, request shape assertions (auth header present, no key in URL, correct endpoint path).

## 3. Runtime wiring

- [x] 3.1 In `cmd/inkflow/main.go`, replace the hard-coded Gemini construction in `loadRuntime` with a switch on `cfg.AI.Provider` (`"gemini"` default / `"openai"`) that builds the corresponding provider client.
- [x] 3.2 Generalize `resolveAPIKey` into provider-specific resolution (e.g. `resolveGeminiAPIKey`, `resolveOpenAIAPIKey`, or one parameterized helper) preserving today's precedence: env var first, then `api_key_file`, with an error naming the active provider, its env var, and its config field.
- [x] 3.3 Parse `cfg.OpenAI.Timeout` the same way `cfg.Gemini.Timeout` is parsed today (`time.ParseDuration`, wrapped error on failure).
- [x] 3.4 Update/extend `cmd/inkflow/main_test.go`: OpenAI selected + `OPENAI_API_KEY` set succeeds; OpenAI selected + no key fails with OpenAI-specific error; Gemini path behavior unchanged; non-AI routes still skip key resolution entirely regardless of provider.

## 4. Documentation

- [x] 4.1 Update `README.md`'s AI/Gemini configuration section to describe both providers, how to pick one, and a short cost/quality comparison table (Gemini 2.5 Flash vs GPT-4.1 vs GPT-4.1 mini) sourced from the researched pricing.
- [x] 4.2 Add an explicit README/AGENTS.md note: ChatGPT Plus/Pro/Codex subscription allowances CANNOT be used in place of an API key — inkflow requires a standalone `OPENAI_API_KEY` (pay-as-you-go API billing), exactly like `GEMINI_API_KEY` today; there is no supported session/cookie-based alternative.
- [x] 4.3 Update `AGENTS.md`'s "Environment Variables" table to add `OPENAI_API_KEY` and note provider-conditional startup key validation.

## 5. Verification

- [x] 5.1 `go build ./cmd/inkflow` succeeds.
- [x] 5.2 `go test ./...` passes, including new/updated tests in `internal/config`, `internal/ai/openai`, and `cmd/inkflow`.
- [x] 5.3 `go test -race ./...` passes for the touched packages.
- [x] 5.4 Manual sanity check (or a table-driven test) confirming an existing `inkflow.toml` with only `[gemini]` and no `[ai]` table still starts and behaves exactly as before this change.
