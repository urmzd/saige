# Examples

Runnable programs demonstrating each saige subsystem. Every example is a standalone `main` package:

```bash
go run ./examples/agent/basic/
go run ./examples/knowledge/basic/
go run ./examples/rag/arxiv/
```

## Quick start

The programs embedded in the root README's Quick Start. Each builds the `default` catalog preset, so it runs with whichever of `ANTHROPIC_API_KEY`, `OPENAI_API_KEY` or Vertex AI credentials is set, or a local Ollama.

| Example | Description |
|---------|-------------|
| [`quickstart/agent`](quickstart/agent/) | The smallest agent, built from a catalog preset |
| [`quickstart/tool`](quickstart/tool/) | A typed tool with `agent.Func` |
| [`quickstart/structured`](quickstart/structured/) | Structured output with `agent.Structured[T]` |
| [`quickstart/dials`](quickstart/dials/) | Creativity and reasoning depth dials |
| [`quickstart/rag`](quickstart/rag/) | Ingest and hybrid search on Postgres, with Ollama embeddings |

## Agent definitions

Definitions in [`agents/`](agents/), run with `saige ask --agents-dir examples/agents --agent NAME`. See [agent definitions](../docs/agent-definitions.md).

| Definition | Description |
|------------|-------------|
| [`agents/assistant.agent.md`](agents/assistant.agent.md) | A concise assistant with the read harness group |
| [`agents/repo-steward.agent.md`](agents/repo-steward.agent.md) | Read-only git commands allowed by approval rules, a preset with a fallback, and the assistant as a sub-agent |

## Agent

| Example | Description |
|---------|-------------|
| [`agent/basic`](agent/basic/) | Single tool with a model served by Ollama |
| [`agent/streaming`](agent/streaming/) | All delta types with ANSI output |
| [`agent/subagents`](agent/subagents/) | Parent delegating to researcher |
| [`agent/concurrent-subagents`](agent/concurrent-subagents/) | Parallel sub-agent execution |
| [`agent/resilient`](agent/resilient/) | Retry + fallback composition |
| [`agent/multimodal`](agent/multimodal/) | File pipeline with `file://` resolver |
| [`agent/caching`](agent/caching/) | Response caching by request hash |
| [`agent/durable`](agent/durable/) | Durable runs: local engine, or duraturo on Postgres |
| [`agent/handoffs`](agent/handoffs/) | Agent-to-agent handoffs |
| [`agent/tui`](agent/tui/) | Interactive and verbose TUI modes |
| [`agent/runner`](agent/runner/) | Multi-turn conversation loop |
| [`agent/approval-grants`](agent/approval-grants/) | Approval policy with an args grant, capability defaults, and a denial limit |
| [`agent/func-tools`](agent/func-tools/) | Typed tools with `agent.Func` and model-computed functions with `agent.AIFunc` |

## Knowledge Graph

| Example | Description |
|---------|-------------|
| [`knowledge/basic`](knowledge/basic/) | Build and query a knowledge graph |

## RAG

| Example | Description |
|---------|-------------|
| [`rag/arxiv`](rag/arxiv/) | Full pipeline with arXiv papers |

## Validation

[`validation/`](validation/) runs live end-to-end checks against real providers. See its [README](validation/README.md).
