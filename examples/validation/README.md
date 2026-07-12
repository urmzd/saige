# SAIGE Live Validation & Benchmarks

End-to-end validation of the agent SDK's features against a **real model**, plus
deterministic micro-benchmarks. The committed sample outputs under
[`results/`](results/) let you see real runs without an API key.

## What it validates

The harness ([`main.go`](main.go)) runs each agent-layer feature against
`gpt-4o-mini` and writes a Markdown report:

| Check | Proves |
|-------|--------|
| `basic_generation` | the streaming agent loop drives a real provider |
| `tool_calling` | the model calls a registered tool and uses its result |
| `response_caching` | an identical request is served from cache (`UsageDelta.CacheHit`, one upstream call) |
| `token_metrics` | `Metrics.RecordTokenUsage` fires with real token counts |
| `agent_handoff` | control transfers between agents on a shared root (`HandoffDelta`) |
| `durable_memoization` | a second durable run replays the recorded LLM step (0 extra API calls) |
| `llm_timeout` | `WithLLMTimeout` cancels a slow provider call |
| `multimodal_tool_output` | a tool returns an image and the model describes it |

## Run it

```bash
export OPENAI_API_KEY=sk-...
just validate                 # or: go run ./examples/validation
just bench-report             # regenerate results/benchmarks.txt
```

Override the model with `SAIGE_VALIDATION_MODEL=gpt-4o-mini`. With no
`OPENAI_API_KEY` the harness prints a notice and exits cleanly (CI-safe).

## Sample outputs (committed)

- [`results/validation-report.md`](results/validation-report.md): last live run (8/8 passing on `gpt-4o-mini`).
- [`results/benchmarks.txt`](results/benchmarks.txt): `go test -bench` numbers for the agent loop, durable path, and cache.

## Benchmarks

Deterministic, mock-based (no network): defined in `agent/bench_test.go` and
`agent/provider/cache/bench_test.go`:

| Benchmark | Measures |
|-----------|----------|
| `BenchmarkAgentTextLoop` | per-turn SDK overhead for a text response |
| `BenchmarkAgentToolLoop` | a full tool round-trip (call → execute → reply) |
| `BenchmarkRunDurableNoop` | overhead the durable seam adds under the no-op runner |
| `BenchmarkKey` | the deterministic cache-key hash (paid on every request) |
| `BenchmarkCacheHit` / `BenchmarkCacheMiss` | cache replay vs. record-and-tee |
