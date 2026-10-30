# Agent framework benchmark

Runs the same six scenarios on saige, Pydantic AI, the OpenAI Agents SDK and
the Claude Agent SDK, all on one Claude model, and compares latency, tokens,
memory, startup and the telemetry each one emits. Results: [results.md](results.md).

## Run it

```bash
docker run -d --rm --name bench-otel -p 4318:4318 \
  -v "$PWD/otel/collector.yaml:/etc/otelcol-contrib/config.yaml" \
  -v "$PWD/results/otel:/out" otel/opentelemetry-collector-contrib:0.120.0
(cd python && uv sync)
ROUNDS=5 ./run_all.sh      # needs ANTHROPIC_API_KEY
python3 report.py          # writes results.md
```

## Method

| Piece | Choice |
|---|---|
| Model | `claude-haiku-5-5`, `max_tokens` 1024, same system prompt everywhere |
| Scenarios | `scenarios.json`: plain answer, one tool, three parallel tools, chained tools, structured output, a tool that raises |
| Order | Interleaved rounds with rotated framework order, so API latency drift hits every framework equally |
| Warmup | One discarded run per scenario per process |
| Tools | The same Python functions (or a Go port) behind each framework's own tool API; the failing tool raises a plain exception |
| Telemetry | Each framework's own OpenTelemetry path into one local collector |

| Framework | Version | Model access | Telemetry path |
|---|---|---|---|
| saige | this repo | Anthropic adapter | `agent/otel.WithTracing` |
| Pydantic AI | 2.63.0 | `anthropic:` model | `Agent.instrument_all()` |
| OpenAI Agents SDK | 0.24.0 | LiteLLM | logfire bridge (native traces go to OpenAI) |
| Claude Agent SDK | 0.2.175 | Claude Code CLI | CLI OTel env vars |

## Caveats

- Five samples per cell. API latency varies by seconds between calls, so per-scenario p50s carry wide error. The overhead table from traces is the stable comparison.
- The Claude Agent SDK runs with built-in tools, setting sources, skills and the default system prompt off. It still sends about 450 extra input tokens per request and starts a CLI subprocess on every `query()`. A long-lived `ClaudeSDKClient` would amortize the startup; this bench does not test that.
- OpenAI Agents reaches Claude through LiteLLM, which adds a translation layer that native OpenAI models skip.
- Pydantic AI treats a plain exception in a tool as fatal by design; tools must raise `ModelRetry` to hand an error back to the model. The `tool_error` failures measure that default, not a crash.
- Startup is time from process start to first agent built. Go's 1 ms excludes `go build`.
- The committed `results.md` was recorded before `otel.Config.CaptureToolContent` existed, so it shows saige without tool arguments and results. The runner now turns capture on; a rerun records them.
