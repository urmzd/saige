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

## Contents

- [Features](#features)
- [Installation](#installation)
- [Quick Start](#quick-start)
- [Concepts in 2 minutes](#concepts-in-2-minutes)
- [Examples](#examples)
- [Documentation](#documentation)
- [Agent Skill](#agent-skill)
- [License](#license)

## Features

saige focuses on three things: running **agents**, supplying their **context and memory**, and measuring both with **evals**.

### Agents

- **Streaming-first agent loop** with typed delta events, parallel tool execution, sub-agent delegation, and handoffs
- **Adapters for three model vendors and a local runtime**: Anthropic, OpenAI and Google, plus open-weight models served by Ollama, all behind one `types.Provider` interface, with retry and fallback composition. See [vendors, runtimes and adapters](docs/concepts.md#vendors-runtimes-and-adapters).
- **Nullable tool properties** with separate presence rules across vendor APIs and MCP. See [tool schemas](docs/tool-schemas.md).
- **Typed function tools**: `agent.Func` derives the schema from a struct, decodes arguments strictly, and passes typed dependencies; `agent.AIFunc` is a typed function a model computes. Both are versioned by content. See [typed function tools](docs/func-tools.md).
- **Durable runs** that resume after a crash, on a local engine or on Postgres through [duraturo](https://github.com/urmzd/duraturo), plus response caching
- **MCP server** exposing any saige tool pack to Claude Code, Codex, Gemini CLI, or any MCP client, with approval enforced for mutating tools
- **Harness toolset**: `agent.WithHarnessTools` adds `read_file`, `list_dir`, `glob`, `grep`, `write_file`, `edit_file`, `execute_code` (shell, Python, Go behind a subprocess or Docker sandbox), `fetch_url`, and scratch tools, read-only unless you enable more, with approvals and automatic spilling of large results. See [harness tools](docs/harness-tools.md).
- **Opt-in tool packs**: workspace files ([`tools/fs`](tools/fs/README.md)), a sandboxed shell ([`tools/exec`](tools/exec/README.md)), and URL fetch with private-address blocking ([`tools/fetch`](tools/fetch/README.md)). Read-only by default; every mutating tool requires approval
- **HTTP and SSE server** via `saige serve`: sessions, a resumable turn event stream in the versioned wire format, and approve and cancel endpoints
- **Model catalog and presets** as data: declared capabilities, layered JSON catalogs loaded from files, HTTPS or any reader, and presets whose failover entries each carry options validated for their own model. See [model catalog and presets](docs/catalog.md).
- **MCP client** with pooled sessions, safe retries, catalog drift detection, and `.mcp.json` loading. See [MCP client](docs/mcp-client.md).
- **Run hooks and guardrails**: one typed seam for lifecycle events that observe, change, or abort a run, recorded for durable replay, and input and output guardrails that pass, block, or rewrite. See [run hooks](docs/hooks.md) and [guardrails](docs/guardrails.md).

### Context and memory

- **Conversation tree** with branching, checkpoints, rewind, compaction, and RLHF feedback
- **Multi-retriever RAG** fusing vector, BM25, and graph retrieval via Reciprocal Rank Fusion, with reranking and citations
- **Long-term memory** scoped by the host, with approval-gated writes and hybrid recall on Postgres, including recall of past conversations. See [memory](docs/memory.md)
- **Knowledge graph backend** for RAG: LLM-powered entity extraction, fuzzy dedup, and temporal tracking. It differs from the document stores in ingestion and retrieval logic, not in role

### Evals

- **Composable scorers** for agents, retrieval, and knowledge graphs, with gates, comparisons, experiments and LLM-as-judge
- **Sampler** to measure how stable scores and subjects are across repeated runs
- **Live eval harness** via `saige eval`

### Why one SDK?

An agent is only as good as the context it is given, and you only know either works if you measure it. **saige** keeps all three under shared `Provider`, `Embedder`, and `Tool` interfaces, so retrieval plugs into the agent loop as tools and every layer is scored by the same eval framework.

## Installation

### What you need

| Requirement | When |
| --- | --- |
| Go 1.26.9 or newer | Library use and `go install` |
| One of `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, Vertex AI credentials (`GOOGLE_GENAI_USE_VERTEXAI=true` and `GOOGLE_CLOUD_PROJECT`) or `GOOGLE_API_KEY`, or a local [Ollama](https://ollama.com) | Always: something has to serve the model |
| PostgreSQL 18 with pgvector and pg_search | Only for RAG, the knowledge graph, and conversation trees or durable runs kept in Postgres |

For those features one Postgres holds vectors, BM25 indexes, conversation trees and durable runs, with no Redis or separate search engine. See [deployment](docs/deployment.md) for the single-Postgres architecture.

### CLI and MCP server

Install a pre-built binary (Linux and macOS, amd64 and arm64, checksum-verified):

```bash
curl -fsSL https://raw.githubusercontent.com/urmzd/saige/main/install.sh | bash
curl -fsSL https://raw.githubusercontent.com/urmzd/saige/main/install.sh | BIN=saige-mcp bash
```

Or build from source:

```bash
go install github.com/urmzd/saige/cmd/saige@latest
go install github.com/urmzd/saige/cmd/saige-mcp@latest
```

Each release also attaches Windows amd64 builds and a `SHA256SUMS` file. Update an installed CLI with `saige update` (`saige update --check` only reports).

### Library

```bash
go get github.com/urmzd/saige
```

## Quick Start

### CLI

```bash
export ANTHROPIC_API_KEY=...   # or OPENAI_API_KEY, or Vertex AI credentials
saige ask "What is retrieval-augmented generation?"
```

Without flags, `saige ask` runs the first vendor whose credentials are set, in the order Anthropic, OpenAI, Google, on its cheapest current model (claude-haiku-5-5, gpt-6-luna, gemini-3.1-flash-lite). Run fully local with a model served by Ollama:

```bash
ollama pull qwen3.5:4b
saige ask --provider ollama --model qwen3.5:4b "What is retrieval-augmented generation?"
```

`saige chat` opens an interactive session. See the [CLI reference](cmd/saige/README.md) for `serve`, RAG, knowledge graph, eval and catalog commands.

### The smallest agent

A catalog preset builds the adapters for you. `default` serves the cheapest model of each vendor whose credentials are set, failing over in order, then a local Ollama model.

<!-- fsrc src="examples/quickstart/agent/main.go" fence="auto" -->
```go
// The smallest agent: a catalog preset instead of a hand-built adapter.
// "default" serves the cheapest model of each vendor whose credentials are
// set (Anthropic, OpenAI, Google), then a local Ollama model, failing over in
// that order.
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/provider/preset"
	"github.com/urmzd/saige/agent/types"
)

func main() {
	ctx := context.Background()
	bundle, err := preset.Build(ctx, catalog.Default(), "default", nil, preset.Options{})
	if err != nil {
		log.Fatal(err)
	}
	defer bundle.Close()

	a := agent.NewAgent(agent.AgentConfig{SystemPrompt: "Answer in one sentence."}, agent.WithPreset(bundle))
	text, err := agent.CollectText(a.Invoke(ctx, []types.Message{types.NewUserMessage("What is RAG?")}))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(text)
}
```
<!-- /fsrc -->

### A typed tool

`agent.Func` derives the tool's JSON schema from a struct and decodes the model's arguments into it.

<!-- fsrc src="examples/quickstart/tool/main.go" fence="auto" -->
```go
// A typed tool: agent.Func derives the JSON schema from WeatherIn and
// decodes the model's arguments into it.
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/provider/preset"
	"github.com/urmzd/saige/agent/types"
)

type WeatherIn struct {
	City string `json:"city" description:"City name"`
}

func main() {
	ctx := context.Background()
	bundle, err := preset.Build(ctx, catalog.Default(), "default", nil, preset.Options{})
	if err != nil {
		log.Fatal(err)
	}
	defer bundle.Close()

	weather := agent.Func("weather", "Current weather for a city",
		func(rc agent.RunContext[agent.NoDeps], in WeatherIn) (string, error) {
			return "18C and sunny in " + in.City, nil
		})

	a := agent.NewAgent(agent.AgentConfig{Tools: types.NewToolRegistry(weather)}, agent.WithPreset(bundle))
	text, err := agent.CollectText(a.Invoke(ctx, []types.Message{types.NewUserMessage("What's the weather in Lisbon?")}))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(text)
}
```
<!-- /fsrc -->

### Structured output

`agent.Structured[T]` returns the answer as a Go value, checked against the schema derived from `T`, and asks the model to repair an invalid answer.

<!-- fsrc src="examples/quickstart/structured/main.go" fence="auto" -->
```go
// Structured output: agent.Structured decodes the answer into a Go struct,
// checks it against the schema derived from the struct, and asks the model
// to repair an invalid answer.
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/provider/preset"
	"github.com/urmzd/saige/agent/types"
)

type Triage struct {
	Priority string `json:"priority" enum:"low,medium,high"`
	Summary  string `json:"summary"`
}

func main() {
	ctx := context.Background()
	bundle, err := preset.Build(ctx, catalog.Default(), "default", nil, preset.Options{})
	if err != nil {
		log.Fatal(err)
	}
	defer bundle.Close()

	a := agent.NewAgent(agent.AgentConfig{SystemPrompt: "Triage support tickets."}, agent.WithPreset(bundle))
	ticket := []types.Message{types.NewUserMessage("Checkout returns HTTP 500 for every customer.")}
	out, _, err := agent.Structured(ctx, a, ticket, agent.OutputSpec[Triage]{Repair: 1})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s: %s\n", out.Priority, out.Summary)
}
```
<!-- /fsrc -->

### Dials

Dials name an intent instead of a vendor parameter, and are compiled for whichever model serves each call, so one setting works across vendors and survives a failover:

```go
agent.WithDials(types.Dials{Creativity: new(types.CreativityFocused), Reasoning: &types.ReasoningDial{Depth: types.DepthHigh}})
```

A dial a model cannot honor is mapped to the nearest value or dropped, and reasoning outranks creativity when a model rules out sampling while it thinks. Full program: [`examples/quickstart/dials`](examples/quickstart/dials/main.go). See [dials](docs/dials.md).

### RAG on Postgres

Start Postgres 18 with pgvector and pg_search, and pull a local embedding model:

```bash
docker run -d --name saige-postgres -p 5432:5432 -e POSTGRES_PASSWORD=postgres paradedb/paradedb:0.26.1-pg18
ollama pull nomic-embed-text
```

Then ingest a document and run a hybrid search (vector similarity fused with BM25):

<!-- fsrc src="examples/quickstart/rag/main.go" fence="auto" -->
```go
// RAG on Postgres: ingest a document, then run a hybrid search that fuses
// pgvector similarity with pg_search BM25. Embeddings come from a local
// Ollama runtime serving nomic-embed-text (768 dimensions, the default
// column size).
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/urmzd/saige/agent/provider/ollama"
	"github.com/urmzd/saige/postgres"
	"github.com/urmzd/saige/rag"
	"github.com/urmzd/saige/rag/embedderregistry"
	"github.com/urmzd/saige/rag/extractor"
	"github.com/urmzd/saige/rag/pgstore"
	ragtypes "github.com/urmzd/saige/rag/types"
)

const dsn = "postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"

func main() {
	ctx := context.Background()
	pool, err := postgres.NewPool(ctx, postgres.Config{URL: dsn})
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	if err := postgres.RunMigrations(ctx, pool, postgres.MigrationOptions{}); err != nil {
		log.Fatal(err)
	}

	emb := ollama.NewEmbedder(ollama.NewClient("http://localhost:11434", "", "nomic-embed-text"))
	pipe, err := rag.NewPipeline(
		rag.WithStore(pgstore.NewStore(pool, nil)),
		rag.WithContentExtractor(extractor.NewAuto()),
		rag.WithEmbedders(embedderregistry.NewTextOnly(textEmbedder{emb})),
		rag.WithRecursiveChunker(512, 64),
		rag.WithBM25(nil), // keyword search runs in Postgres through pg_search
	)
	if err != nil {
		log.Fatal(err)
	}
	defer pipe.Close(ctx)

	doc := "saige stores vectors, BM25 indexes and conversation trees in one Postgres 18 database."
	if _, err := pipe.Ingest(ctx, &ragtypes.RawDocument{SourceURI: "notes://saige", MIMEType: "text/plain", Data: []byte(doc)}); err != nil {
		log.Fatal(err)
	}
	res, err := pipe.Search(ctx, "Which database does saige use?", ragtypes.WithLimit(3))
	if err != nil {
		log.Fatal(err)
	}
	for _, hit := range res.Hits {
		fmt.Printf("%.3f %s\n", hit.Score, hit.Variant.Text)
	}
}

// textEmbedder embeds the text of each content variant.
type textEmbedder struct{ e *ollama.OllamaEmbedder }

func (t textEmbedder) Embed(ctx context.Context, variants []ragtypes.ContentVariant) ([][]float32, error) {
	texts := make([]string, len(variants))
	for i, v := range variants {
		texts[i] = v.Text
	}
	return t.e.Embed(ctx, texts)
}
```
<!-- /fsrc -->

Run any of these with `go run ./examples/quickstart/<name>` from a clone.

## Concepts in 2 minutes

| Piece | What it is | Guide |
| --- | --- | --- |
| **Agent** | The loop: model turn, tool calls, results, repeat | [agent](agent/README.md) |
| **Adapter** | A `types.Provider` for one serving API: a vendor (Anthropic, OpenAI, Google) or a runtime (Ollama) | [concepts](docs/concepts.md#vendors-runtimes-and-adapters) |
| **Catalog and presets** | What each model accepts, and named chains of complete configurations | [catalog](docs/catalog.md) |
| **Dials** | Model-neutral settings compiled per attempt | [dials](docs/dials.md) |
| **Tools** | Typed Go functions, MCP imports and tool packs; parallelism and tool choice | [tool calling](docs/tool-calling.md), [typed tools](docs/func-tools.md) |
| **Delegation** | Handoffs and sub-agents | [delegation](docs/delegation.md) |
| **RAG** | Ingest, chunk, embed, hybrid search on Postgres, citations | [rag](rag/README.md) |
| **Evals** | Scorers, gates and experiments for agents, retrieval and graphs | [eval](eval/README.md) |
| **Durable runs** | Journaled steps that resume after a crash, on a local engine or Postgres | [durable execution](docs/durable-execution.md) |

See [concepts](docs/concepts.md) for the full picture.

## Examples

See [`examples/`](examples/README.md) for runnable programs covering sub-agents, handoffs, durability, caching, approvals, knowledge graphs and RAG pipelines.

## Documentation

Every guide is listed in the [docs index](docs/README.md). Each package also has its own README:

| Package | Documentation | Covers |
|---------|---------------|--------|
| `agent` | [agent/README.md](agent/README.md) | Adapters, deltas, tools, sub-agents, markers, conversation tree, RLHF feedback, TUI, testing |
| `rag` | [rag/README.md](rag/README.md) | Data model, chunking, retrieval, reranking, HyDE, metrics, tool bindings |
| `rag/knowledge` | [rag/knowledge/README.md](rag/knowledge/README.md) | Knowledge graph backend: graph interface, hybrid search, deduplication, PostgreSQL backend, formatting |
| `eval` | [eval/README.md](eval/README.md) | Scorers, gates, comparisons, experiments, LLM-as-judge, stream timing, live eval harness (`saige eval`) |
| `cmd/saige` | [cmd/saige/README.md](cmd/saige/README.md) | CLI reference: chat, ask, serve, rag, kg, eval |
| `cmd/saige-mcp` | [cmd/saige-mcp/README.md](cmd/saige-mcp/README.md) | MCP server setup for Claude Code, Codex, Gemini CLI |
| `tools/research` | [tools/research/README.md](tools/research/README.md) | Web search, file, and knowledge graph tools |
| `tools` | [docs/harness-tools.md](docs/harness-tools.md) | Harness toolset: groups, sandboxes, approvals, spill, CLI `--tools` |
| `tools/fs` | [tools/fs/README.md](tools/fs/README.md) | Workspace read, list, glob, grep, write, edit with root confinement |
| `tools/exec` | [tools/exec/README.md](tools/exec/README.md) | Sandboxed bash and execute_code with command, environment, and network policy; subprocess and Docker sandboxes |
| `tools/fetch` | [tools/fetch/README.md](tools/fetch/README.md) | URL fetch through a client that blocks private and metadata addresses |
| `examples` | [examples/README.md](examples/README.md) | Runnable example index |

API reference for every package: [pkg.go.dev/github.com/urmzd/saige](https://pkg.go.dev/github.com/urmzd/saige)

## Agent Skill

This repo's conventions are available as portable agent skills in [`skills/`](skills/).

## License

Apache 2.0. See [LICENSE](LICENSE).
