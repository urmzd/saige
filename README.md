<p align="center">
  <h1 align="center">saige</h1>
  <p align="center">
    <strong>Super Artificial Intelligence Graph Environment</strong>
    <br />
    A Go SDK for building AI agents, giving them context and memory, and evaluating them.
    <br /><br />
    <a href="https://pkg.go.dev/github.com/urmzd/saige">Install</a>
    &middot;
    <a href="https://github.com/urmzd/saige/issues">Report Bug</a>
    &middot;
    <a href="https://pkg.go.dev/github.com/urmzd/saige">Go Docs</a>
  </p>
</p>

<p align="center">
  <a href="https://github.com/urmzd/saige/actions/workflows/ci.yml"><img src="https://github.com/urmzd/saige/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://pkg.go.dev/github.com/urmzd/saige"><img src="https://pkg.go.dev/badge/github.com/urmzd/saige.svg" alt="Go Reference"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache%202.0-blue.svg" alt="License"></a>
</p>

<p align="center">
  <img src="showcase/agent-basic.png" alt="Basic agent demo" width="80%">
</p>

## Features

saige focuses on three things: running **agents**, supplying their **context and memory**, and measuring both with **evals**.

### Agents

- **Streaming-first agent loop** with typed delta events, parallel tool execution, sub-agent delegation, and handoffs
- **4 LLM providers** (Ollama, OpenAI, Anthropic, Google) behind one `Provider` interface, with retry and fallback composition
- **Nullable tool properties** with separate presence rules across providers and MCP. See [tool schemas](docs/tool-schemas.md).
- **Durable runs** that resume after a crash, plus response caching
- **MCP server** exposing any saige tool pack to Claude Code, Codex, Gemini CLI, or any MCP client

### Context and memory

- **Conversation tree** with branching, checkpoints, rewind, compaction, and RLHF feedback
- **Multi-retriever RAG** fusing vector, BM25, and graph retrieval via Reciprocal Rank Fusion, with reranking and citations
- **Knowledge graph backend** for RAG: LLM-powered entity extraction, fuzzy dedup, and temporal tracking. It differs from the document stores in ingestion and retrieval logic, not in role

### Evals

- **Composable scorers** for agents, retrieval, and knowledge graphs, with A/B experiments and LLM-as-judge
- **Sampler** to measure how stable scores and subjects are across repeated runs
- **Live eval harness** via `saige eval`

### Why one SDK?

An agent is only as good as the context it is given, and you only know either works if you measure it. **saige** keeps all three under shared `Provider`, `Embedder`, and `Tool` interfaces, so retrieval plugs into the agent loop as tools and every layer is scored by the same eval framework.

## Installation

### Library

```bash
go get github.com/urmzd/saige
```

### CLI and MCP server

```bash
go install github.com/urmzd/saige/cmd/saige@latest
go install github.com/urmzd/saige/cmd/saige-mcp@latest
```

Or install a pre-built `saige` binary:

```bash
curl -fsSL https://raw.githubusercontent.com/urmzd/saige/main/install.sh | sh
```

## Quick Start

### CLI

```bash
saige chat                                    # interactive multi-turn chat
saige ask "What is retrieval-augmented generation?"

# Serve saige tools to Claude Code, Codex, or Gemini CLI over MCP
saige-mcp --tools all --db "$SAIGE_DB" --searxng-url http://localhost:8080
```

The CLI auto-detects a provider from `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, or `GOOGLE_API_KEY`, falling back to Ollama (no key needed). See the [CLI reference](cmd/saige/README.md) for RAG/KG subcommands and flags.

### Library

```go
import (
    "github.com/urmzd/saige/agent"
    "github.com/urmzd/saige/agent/types"
    "github.com/urmzd/saige/agent/provider/ollama"
)

client := ollama.NewClient("http://localhost:11434", "qwen2.5", "nomic-embed-text")
a := agent.NewAgent(agent.AgentConfig{
    Name:         "assistant",
    SystemPrompt: "You are a helpful assistant.",
    Provider:     ollama.NewAdapter(client),
    Tools:        types.NewToolRegistry(myTool),
})

stream := a.Invoke(ctx, []types.Message{types.NewUserMessage("Hello!")})
for delta := range stream.Deltas() {
    switch d := delta.(type) {
    case types.TextContentDelta:
        fmt.Print(d.Content)
    }
}
```

See [`examples/`](examples/) for runnable programs covering knowledge graphs, RAG pipelines, sub-agents, durability, and more.

## Documentation

Each subsystem has its own README as the entrypoint for further information:

| Package | Documentation | Covers |
|---------|---------------|--------|
| `agent` | [agent/README.md](agent/README.md) | Providers, deltas, tools, sub-agents, markers, conversation tree, RLHF feedback, TUI, testing |
| `rag` | [rag/README.md](rag/README.md) | Data model, chunking, retrieval, reranking, HyDE, metrics, tool bindings |
| `rag/knowledge` | [rag/knowledge/README.md](rag/knowledge/README.md) | Knowledge graph backend: graph interface, hybrid search, deduplication, PostgreSQL backend, formatting |
| `eval` | [eval/README.md](eval/README.md) | Scorers, A/B experiments, LLM-as-judge, stream timing, live eval harness (`saige eval`) |
| `cmd/saige` | [cmd/saige/README.md](cmd/saige/README.md) | CLI reference: chat, ask, rag, kg, eval |
| `cmd/saige-mcp` | [cmd/saige-mcp/README.md](cmd/saige-mcp/README.md) | MCP server setup for Claude Code, Codex, Gemini CLI |
| `tools/research` | [tools/research/README.md](tools/research/README.md) | Web search, file, and knowledge graph tools |
| `examples` | [examples/README.md](examples/README.md) | Runnable example index |

API reference for every package: [pkg.go.dev/github.com/urmzd/saige](https://pkg.go.dev/github.com/urmzd/saige)

## Agent Skill

This repo's conventions are available as portable agent skills in [`skills/`](skills/).

## Orchestration contracts

See [ownership and policies](docs/orchestration-policies.md) for subagent results, handoff return links, sticky routing, and approval limits.
See [cache contracts](docs/cache-contracts.md) for cache identity, provider cache modes, and remaining defects.
[Design decisions](DESIGN_DECISIONS.md) explain the choices and their limits.

## License

Apache 2.0. See [LICENSE](LICENSE).
