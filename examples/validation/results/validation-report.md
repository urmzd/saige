# SAIGE Live Validation Report

Model: `gpt-4o-mini` (OpenAI) — real end-to-end runs of the agent SDK.

**8 passed, 0 failed, 0 skipped** of 8 feature checks.

| Feature | Result | Detail | Time |
|---|---|---|---|
| `basic_generation` | ✅ PASS | model replied: The capital of France is Paris. | 786ms |
| `tool_calling` | ✅ PASS | tool called; model answered 391 | 1.281s |
| `response_caching` | ✅ PASS | identical request served from cache (1 upstream call, CacheHit=true) | 490ms |
| `token_metrics` | ✅ PASS | recorded usage: 17 input + 2 output tokens | 938ms |
| `agent_handoff` | ✅ PASS | control transferred triage → math | 1.489s |
| `durable_memoization` | ✅ PASS | second durable run replayed the memoized LLM step (0 extra API calls) | 815ms |
| `llm_timeout` | ✅ PASS | 1ms LLM timeout fired as expected: provider openai (model ): context deadline exceeded | 2ms |
| `multimodal_tool_output` | ✅ PASS | model received the tool's image and identified it as red | 1.593s |

> Regenerate with `OPENAI_API_KEY=... go run ./examples/validation`.
