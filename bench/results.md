# Agent framework benchmark

Model `claude-haiku-5-5` for every framework. 120 measured runs in interleaved rounds. Latency is wall time for one full agent run, TTFT is time to the first streamed model delta.

## Summary

| Framework | Pass | p50 latency | p50 TTFT | Input tok / run | Output tok / run | Requests / run | Startup | Peak RSS |
|---|---|---|---|---|---|---|---|---|
| **saige** | 30/30 | 1,626 ms | 690 ms | 935 | 99 | 1.8 | 1 ms | 47 MB |
| **pydantic-ai** | 25/30 | 2,124 ms | 679 ms | 728 | 79 | 1.5 | 991 ms | 158 MB |
| **openai-agents** | 30/30 | 1,695 ms | 671 ms | 931 | 112 | 1.8 | 1,906 ms | 300 MB |
| **claude-agent-sdk** | 30/30 | 2,667 ms | 1,295 ms | 1,942 | 112 | 1.8 | 470 ms | 403 MB |

## Latency by scenario (p50 / max, ms)

| Scenario | saige | pydantic-ai | openai-agents | claude-agent-sdk |
|---|---|---|---|---|
| plain | 689 / 7,039 | 862 / 1,794 | 743 / 7,106 | 2,018 / 2,120 |
| single_tool | 1,699 / 2,201 | 4,399 / 7,163 | 1,556 / 1,682 | 2,772 / 4,336 |
| parallel_tools | 1,609 / 2,590 | 2,971 / 5,015 | 1,738 / 1,843 | 3,092 / 3,803 |
| chained_tools | 4,056 / 5,327 | 2,212 / 3,330 | 2,858 / 3,154 | 4,088 / 6,523 |
| structured | 866 / 2,560 | 3,505 / 4,126 | 1,704 / 1,978 | 2,095 / 2,333 |
| tool_error | 2,210 / 2,402 | 911 / 1,437 | 1,944 / 12,414 | 2,653 / 3,249 |

## Pass rate and tokens by scenario (pass, mean input/output tokens)

| Scenario | saige | pydantic-ai | openai-agents | claude-agent-sdk |
|---|---|---|---|---|
| plain | 5/5, 48/12 | 5/5, 48/12 | 5/5, 48/12 | 5/5, 499/12 |
| single_tool | 5/5, 893/69 | 5/5, 899/69 | 5/5, 911/69 | 5/5, 2,012/75 |
| parallel_tools | 5/5, 1,107/195 | 5/5, 1,113/195 | 5/5, 1,062/194 | 5/5, 2,261/218 |
| chained_tools | 5/5, 2,018/174 | 5/5, 2,012/174 | 5/5, 1,980/174 | 5/5, 3,761/195 |
| structured | 5/5, 577/81 | 5/5, 296/27 | 5/5, 607/81 | 5/5, 1,046/100 |
| tool_error | 5/5, 965/63 | 0/5, 0/0 | 5/5, 979/140 | 5/5, 2,074/70 |

## Failures

| Framework | Scenario | Run | Reason |
|---|---|---|---|
| pydantic-ai | tool_error | 5x | RuntimeError: upstream quote service unavailable |

## Framework overhead (from traces)

Root span time minus model-call time, per run. This removes API speed from the comparison. The Claude Agent SDK's spans overlap with a background title request, so its in-process number is not meaningful; its startup cost shows up as wall time minus the others on `plain`.

| Framework | p50 | p90 |
|---|---|---|
| saige | 1 ms | 2 ms |
| pydantic-ai | 5 ms | 16 ms |
| openai-agents | 7 ms | 17 ms |
| claude-agent-sdk | ~1,307 ms per query (CLI subprocess) | n/a |

## Observability (from the shared OTel collector, warmups included)

| Signal | saige | pydantic-ai | openai-agents | claude-agent-sdk |
|---|---|---|---|---|
| spans / run | 6.0 | 5.7 | 8.0 | 8.1 |
| tokens on model spans | yes | yes | yes | yes |
| TTFT on model spans | yes | yes | no | yes |
| tool arguments | no | yes | yes | no |
| tool results | no | yes | yes | no |
| tool error on span | yes | yes | yes | yes |
| prompt / response content | no / no | yes / yes | yes / yes | yes / no |
| hidden model calls | 0 | 0 | 0 | 48 |
| OTel logs | 0 | 0 | 0 | 736 |
| OTel metrics | 0 | 0 | 0 | 4 |
| GenAI semconv span names | yes | yes | no | no |

### Span names

- **saige**: `anthropic.messages.create` (110), `chat claude-haiku-5-5` (110), `invoke_agent` (60), `execute_tool get_weather` (40), `execute_tool get_population` (20), `execute_tool multiply` (10), `execute_tool get_stock` (10)
- **pydantic-ai**: `anthropic.messages.create` (100), `chat claude-haiku-5-5` (100), `invoke_agent agent` (60), `execute_tool get_weather` (40), `execute_tool get_population` (20), `execute_tool multiply` (10), `execute_tool get_stock` (10)
- **openai-agents**: `Chat completion with {gen_ai.request.model!r}` (110), `Turn {turn} for agent bench` (110), `Function: {name}` (80), `Agent run: {name!r}` (60), `Task: Agent workflow` (60), `OpenAI Agents trace: {name}` (60)
- **claude-agent-sdk**: `claude_code.llm_request` (158), `claude_code.tool.blocked_on_user` (90), `claude_code.tool.execution` (90), `claude_code.tool` (90), `claude_code.interaction` (60)
