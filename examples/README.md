# Examples

Runnable programs demonstrating each saige subsystem. Every example is a standalone `main` package:

```bash
go run ./examples/agent/basic/
go run ./examples/knowledge/basic/
go run ./examples/rag/arxiv/
```

## Agent

| Example | Description |
|---------|-------------|
| [`agent/basic`](agent/basic/) | Single tool with Ollama |
| [`agent/streaming`](agent/streaming/) | All delta types with ANSI output |
| [`agent/subagents`](agent/subagents/) | Parent delegating to researcher |
| [`agent/concurrent-subagents`](agent/concurrent-subagents/) | Parallel sub-agent execution |
| [`agent/resilient`](agent/resilient/) | Retry + fallback composition |
| [`agent/multimodal`](agent/multimodal/) | File pipeline with `file://` resolver |
| [`agent/caching`](agent/caching/) | Response caching by request hash |
| [`agent/durable`](agent/durable/) | DBOS-backed durable runs |
| [`agent/handoffs`](agent/handoffs/) | Agent-to-agent handoffs |
| [`agent/tui`](agent/tui/) | Interactive and verbose TUI modes |
| [`agent/runner`](agent/runner/) | Multi-turn conversation loop |

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
