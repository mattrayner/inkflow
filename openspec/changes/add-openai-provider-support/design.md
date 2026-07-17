## Context

Inkflow's importer (`internal/importer/importer.go`) already talks to AI through a narrow interface, `ai.Provider.Process(ctx, io.Reader) (ai.Result, error)`. The only implementation is `internal/ai/gemini`, and every touchpoint outside that package is Gemini-specific: `config.GeminiConfig` is a top-level `Config` field, `cmd/inkflow/main.go` always builds a `gemini.Client` when any route has `ai = true`, and `resolveAPIKey` only knows about `GEMINI_API_KEY` / `[gemini].api_key_file`.

The user's two questions:
1. Would an OpenAI model be a better/cheaper fit for OCR of handwritten PDFs? Research (see proposal) says GPT-4.1 is a viable quality-focused alternative and GPT-4.1 mini is a viable cost-focused alternative; Gemini 2.5 Flash remains the cheapest well-supported default. There is no single "correct" answer — it depends on the user's handwriting and cost tolerance — so the right move is to make the provider swappable, not to replace Gemini outright.
2. Can we use ChatGPT/Codex subscription allowances instead of API tokens? No — confirmed via OpenAI's own billing docs that ChatGPT plans and the API are separately billed products with no supported bridge. This is a hard constraint, not an implementation detail, and this design treats "OPENAI_API_KEY-based billing" as the only supported path.

## Goals / Non-Goals

**Goals:**
- Let a route's AI processing be backed by either Gemini or OpenAI, chosen via config, with no code changes required to switch.
- Keep `ai.Provider` as the sole extension point; the importer stays provider-agnostic.
- Preserve exact current behavior (config shape, defaults, error text) when `[ai].provider` is unset or `"gemini"` — zero-touch upgrade for existing users.
- Match Gemini's existing safety properties in the OpenAI client: API key never appears in URLs/query strings, structured (schema-validated) JSON output rather than free-text parsing, clear error surfacing into marker blocks, no retry-on-failure (matches documented "no Gemini retry" behavior — consistent behavior across providers).
- Fail fast at startup if a route wants AI but the selected provider has no resolvable key (mirrors current Gemini behavior).

**Non-Goals:**
- No ChatGPT/Codex subscription-based auth. Explicitly out of scope; documented as unsupported, not attempted.
- No multi-provider fan-out / fallback-to-other-provider-on-failure. One active provider per running server.
- No streaming responses, no chat history, no non-PDF inputs.
- No automatic model recommendation/benchmarking system — model choice is a static config value the user sets deliberately.
- No change to dedup, marker-block replace-not-append, or template rendering behavior.

## Decisions

**1. Config shape: new `[ai]` table with `provider`, existing `[gemini]` table kept as-is, new `[openai]` table added.**
Alternative considered: fold everything into one generic `[ai]` table with provider-prefixed keys (e.g. `ai.gemini_model`, `ai.openai_model`). Rejected — it's a bigger breaking change for zero benefit; keeping `[gemini]`/`[openai]` as separate tables matches the existing mental model (one table per provider) and requires no migration for current users, since `[gemini]` is untouched.
```toml
[ai]
provider = "openai"   # "gemini" (default) | "openai"

[gemini]
# unchanged — model, api_key_file, timeout, prompts

[openai]
api_key_file = "..."
model = "gpt-4.1"      # default; gpt-4.1-mini for cost-sensitive users
timeout = "60s"
ocr_prompt = "..."
summary_prompt = "..."
```
`Config.AI.Provider` defaults to `"gemini"` in `applyDefaults`, so an empty/absent `[ai]` table is 100% backward compatible.

**2. Provider construction: a small factory in `cmd/inkflow/main.go`, not a registry/plugin system.**
Alternative considered: a `map[string]ProviderFactory` registry for extensibility. Rejected as premature — two providers don't justify indirection; a `switch cfg.AI.Provider { case "gemini": ...; case "openai": ... }` in `loadRuntime` is simpler to read and test, and can be upgraded to a registry later if a third provider appears.

**3. OpenAI client implementation mirrors `internal/ai/gemini` structure exactly.**
New package `internal/ai/openai` with `ClientConfig` (Endpoint, APIKey, Model, Timeout, OCRPrompt, SummaryPrompt) and `Client.Process`. Uses the OpenAI Responses API (`POST /v1/responses`) with:
  - `input`: one user message containing the combined OCR+summary prompt text plus a `input_file` content part carrying the base64-encoded PDF (`file_data` + `filename`), matching how OpenAI's Responses API accepts inline file input without a separate upload round-trip.
  - `text.format`: a strict JSON schema requiring `ocr_text` (string) and `summary` (array of strings) — same contract as Gemini's `response_schema`, so `ai.Result` construction is identical.
  - Auth via `Authorization: Bearer <key>` header (never in URL), matching Gemini's rationale for keeping keys out of transport-error strings.
  - Same "no retry" policy: a single request per `Process` call; errors bubble up for the importer to write into marker blocks, consistent with documented Gemini behavior.
Alternative considered: use OpenAI's older Chat Completions API with `image_url` per-page. Rejected — Chat Completions does not accept PDFs directly (would require a page-image conversion step, which is far more implementation and cost than the Responses API's native file input), and Responses API is OpenAI's current recommended surface per official docs.

**4. API key resolution generalized behind a small per-provider helper, not a single "any key" function.**
`resolveAPIKey` becomes provider-aware: `resolveGeminiAPIKey`/`resolveOpenAIAPIKey` (or one function parameterized by env-var name + config file field), each following the existing precedence (env var first, then `api_key_file`). Error message names the active provider and its expected env var, e.g. `openai: no API key — set $OPENAI_API_KEY or [openai].api_key_file`.

**5. Startup validation only checks the *selected* provider's key.**
If `provider = "openai"` and only `GEMINI_API_KEY` is set, startup must fail (matches today's fail-fast philosophy) rather than silently using an unrelated key. If no route has `ai = true`, no key is required from either provider, unchanged from today.

**6. Documentation explicitly states the ChatGPT/Codex allowance limitation.**
README/AGENTS.md gain a short section: ChatGPT Plus/Pro/Codex subscription allowances are billed separately from the API and cannot be consumed by a third-party server; inkflow requires a standalone `OPENAI_API_KEY` (pay-as-you-go API billing) exactly like it requires `GEMINI_API_KEY` today. This prevents the question from being re-raised as a bug report later.

## Risks / Trade-offs

- **[Risk] OpenAI Responses API shape or model names change again before implementation lands.** → Mitigation: `internal/ai/openai/client_test.go` mocks the HTTP layer (same pattern as `gemini/client_test.go`), so the contract is pinned by tests; the model name is a config default, trivially updated later without code changes.
- **[Risk] Handwriting OCR quality/cost differs meaningfully by document; a static default (`gpt-4.1`) may not suit every user.** → Mitigation: model is a config field the user sets, not hard-coded; README documents the GPT-4.1 vs GPT-4.1 mini vs Gemini trade-off so users can choose deliberately, and can change it without a code change.
- **[Risk] Two provider packages now need to stay behaviorally consistent (error formatting, JSON parsing edge cases like double-escaped newlines) or the importer's marker-block output will differ oddly by provider.** → Mitigation: share `ai.Result` construction contract; add equivalent `unescapeNewlines`-style handling in the OpenAI client if the same double-escaping is observed in practice; both client test suites assert on `ai.Result` shape, not raw provider JSON.
- **[Risk] Users may still try to use a ChatGPT session/cookie thinking it's supported.** → Mitigation: explicit "not supported" documentation, worded clearly in README/AGENTS.md, config only ever accepts an API key field — no config surface implies a session-based auth path exists.
- **[Trade-off] No shared "prompt building" helper extracted between providers even though both concatenate OCR+summary prompts similarly.** Accepted for this change — premature abstraction across two small, independently-testable clients; can be revisited if a third provider repeats the same logic.

## Migration Plan

- Purely additive at the config level: existing `inkflow.toml` files with `[gemini]` and no `[ai]` table continue to work unchanged (provider defaults to `"gemini"`).
- Users who want OpenAI add `[ai] provider = "openai"`, a `[openai]` table, and set `OPENAI_API_KEY` (or `[openai].api_key_file`).
- No data migration — `state.db` records and vault files are provider-agnostic already (marker blocks just hold text).
- Rollback: revert `[ai].provider` to `"gemini"` (or remove the `[ai]` table) and restart; no other state changes needed.

## Open Questions

- None blocking — default OpenAI model (`gpt-4.1` vs `gpt-4.1-mini`) is a config default choice made in tasks.md/implementation, not an open architectural question.
