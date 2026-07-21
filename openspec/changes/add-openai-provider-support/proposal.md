## Why

Gemini 2.5 Flash is the only supported AI backend for OCR + summarization of handwritten BOOX PDFs, and its cost for image-heavy handwritten pages is high enough that the user wants a cheaper or better-suited alternative. OpenAI's vision-capable models (GPT-4.1 / GPT-4.1 mini) are candidates, but inkflow's AI integration is hard-wired to Gemini's request/response shape end-to-end (config, client, main.go wiring), so nothing can be swapped without a refactor.

Separately, the user asked whether a ChatGPT Plus/Pro or Codex subscription allowance could be used instead of paying for API tokens. It cannot: ChatGPT/Codex subscriptions and the OpenAI API are billed and provisioned separately, and there is no supported way for a third-party server like inkflow to spend a user's ChatGPT/Codex allowance instead of an `OPENAI_API_KEY`. This proposal documents that constraint so it isn't re-investigated later, and designs the config/docs so users aren't misled into thinking a ChatGPT login will work.

## What Changes

- Generalize the single-provider Gemini config into a provider-selectable AI config (`[ai]` section with `provider = "gemini" | "openai"`), keeping Gemini as the default so existing configs keep working.
- Add a new `internal/ai/openai` package implementing the existing `ai.Provider` interface against the OpenAI Responses API (PDF input via base64/file upload, structured JSON output for `ocr_text` + `summary`), modeled directly on `internal/ai/gemini`.
- Add `OPENAI_API_KEY` env var + `[openai].api_key_file` config fallback, following the same precedence pattern as `GEMINI_API_KEY`.
- Update `cmd/inkflow/main.go` runtime wiring to construct whichever provider is selected instead of always constructing Gemini.
- Update config validation: if any route has `ai = true`, the selected provider must have a resolvable API key at startup (same fail-fast behavior as today, generalized across providers).
- Document in README/AGENTS.md/example TOML that **no ChatGPT/Codex subscription can substitute for an API key** — only a billed OpenAI (or Google) API key works — and give a cost/quality comparison so users can pick a provider deliberately.
- **BREAKING (config shape only, not behavior)**: introduces `[ai].provider`; the existing `[gemini]` table remains valid and is still the default, so unmodified existing configs continue to work unchanged.

## Capabilities

### New Capabilities
- `ai-provider-selection`: Config-driven selection between multiple AI providers (Gemini, OpenAI) for OCR + summary generation, with per-provider API key resolution and fail-fast validation.
- `openai-ocr-provider`: An `ai.Provider` implementation backed by the OpenAI API that transcribes a handwritten PDF and produces a short summary in the same structured shape Gemini produces today.

### Modified Capabilities
(none — no existing `openspec/specs/` capabilities predate this change)

## Impact

- `internal/config/types.go`, `internal/config/load.go`: new `[ai]` table, provider enum/default, validation changes.
- `cmd/inkflow/main.go`: provider construction and API key resolution generalized.
- `internal/ai/openai/` (new): client, config, request/response types, tests mirroring `internal/ai/gemini`.
- `internal/ai/provider.go`: unchanged interface, reused by the new provider.
- `internal/importer/importer.go`: no change expected (already provider-agnostic via `ai.Provider`).
- `README.md`, `AGENTS.md`, `inkflow.example.toml`: document provider choice, cost tradeoffs, and the ChatGPT/Codex allowance limitation.
- Tests: `internal/config/load_test.go`, `cmd/inkflow/main_test.go` need cases for the new config shape and provider selection.
