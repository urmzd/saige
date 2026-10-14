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

Build agents, supply their context and memory, and evaluate the results through shared Go interfaces.

- **Streaming agents** with typed message parts, parallel tool calls, sub-agents and handoffs. Use [typed Go functions](docs/func-tools.md), [MCP tools](docs/mcp-client.md) or the built-in [harness tools](docs/harness-tools.md).
- **Model adapters and routing** for Anthropic, OpenAI, Google and local Ollama, with [catalog presets](docs/catalog.md), retry, fallback and model-neutral [dials](docs/dials.md).
- **Versioned agent definitions** loaded from Markdown, directories, HTTPS or Postgres. Share one definition across the CLI, HTTP server, MCP and [editor harnesses](docs/harnesses.md). See [agent definitions](docs/agent-definitions.md).
- **Approvals and guardrails** for tool execution and model input/output, with [approval policies](docs/orchestration-policies.md) and [guardrails](docs/guardrails.md).
- **Context and memory** through branching conversations, hybrid vector/BM25/graph retrieval, citations and approval-gated [long-term memory](docs/memory.md). See [RAG](rag/README.md) and [knowledge graphs](rag/knowledge/README.md).
- **Durable execution** with resumable local or Postgres-backed runs, response caching and [batch jobs](docs/batch.md). See [durable runs](docs/durable-execution.md).
- **Evals** with composable scorers, regression gates, comparisons, experiments and LLM judges. Run suites with `saige eval` or use the [Go API](eval/README.md).

## Installation

### Prerequisites

| Requirement | When |
| --- | --- |
| Go 1.26.9 or newer | Library use and `go install` |
| One of `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, Vertex AI credentials (`GOOGLE_GENAI_USE_VERTEXAI=true` and `GOOGLE_CLOUD_PROJECT`) or `GOOGLE_API_KEY`, or a local [Ollama](https://ollama.com) | Model-backed agent calls and live evals |
| PostgreSQL 18 with pgvector and pg_search | Postgres-backed retrieval, memory, conversation trees and durable runs |

Basic agent calls need no database. RAG also supports an in-memory store. For persistent deployments, one Postgres can hold vectors, BM25 indexes, conversation trees and durable runs. See [deployment](docs/deployment.md) for the single-Postgres architecture.

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

Without flags, `saige ask` uses the default catalog preset and available credentials. See [catalog presets](docs/catalog.md) for model selection and failover. Run fully local with a model served by Ollama:

```bash
ollama pull qwen3.5:4b
saige ask --provider ollama "What is retrieval-augmented generation?"
```

`--provider ollama` serves `qwen3.5:4b`, or any other chat model you have pulled.

`saige chat` opens an interactive session. See the [CLI reference](cmd/saige/README.md) for `serve`, RAG, knowledge graph, eval and catalog commands.

### The smallest agent

Run the checked-in example from a clone, using your configured provider credentials or a running Ollama server with a chat model pulled:

```bash
git clone https://github.com/urmzd/saige.git
cd saige
go run ./examples/quickstart/agent
```

The example builds the `default` catalog preset, invokes one agent turn and prints the answer:

<!-- fsrc src="examples/quickstart/agent/main.go" fence="auto" -->
```go
// The smallest agent: a catalog preset instead of a hand-built adapter.
// "default" serves the cheapest model of each vendor whose credentials are
// set (Anthropic, OpenAI, Google), then a model pulled into a local Ollama,
// failing over in that order.
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
	defer bundle.Close(ctx)

	a, err := agent.New(agent.Config{SystemPrompt: "Answer in one sentence."}, agent.WithPreset(bundle))
	if err != nil {
		log.Fatal(err)
	}
	text, err := agent.CollectText(a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("What is RAG?"))}))
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
	defer bundle.Close(ctx)

	weather := agent.Func("weather", "Current weather for a city",
		func(rc agent.RunContext[agent.NoDeps], in WeatherIn) (string, error) {
			return "18C and sunny in " + in.City, nil
		})

	a, err := agent.New(agent.Config{Tools: types.NewToolRegistry(weather)}, agent.WithPreset(bundle))
	if err != nil {
		log.Fatal(err)
	}
	text, err := agent.CollectText(a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("What's the weather in Lisbon?"))}))
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
	defer bundle.Close(ctx)

	a, err := agent.New(agent.Config{SystemPrompt: "Triage support tickets."}, agent.WithPreset(bundle))
	if err != nil {
		log.Fatal(err)
	}
	ticket := []types.Message{types.UserMsg(types.Text("Checkout returns HTTP 500 for every customer."))}
	out, _, err := agent.Structured(ctx, a, ticket, agent.OutputSpec[Triage]{Repair: 1})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s: %s\n", out.Priority, out.Summary)
}
```
<!-- /fsrc -->

### Images and documents

A message is an ordered list of typed parts. Media is a part whose `Source` holds bytes, an `https` URL, a workspace reference or a vendor upload:

```go
msg := types.UserMsg(types.Text("What is in this image?"), types.Image(types.Bytes(types.MediaPNG, data)))
```

A part the serving model cannot take is rejected unless you permit a conversion, such as extracting a PDF's text. Full programs: [`examples/parts`](examples/parts/). See [message parts](docs/parts.md) and [modality conversion](docs/modality-conversion.md).

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

Then ingest a document and run a hybrid search (vector similarity fused with BM25). `embedderregistry.Text` turns any adapter's text embedder into the pipeline's embedder:

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

	client, err := ollama.NewClient(ollama.Config{Host: "http://localhost:11434", EmbeddingModel: "nomic-embed-text"})
	if err != nil {
		log.Fatal(err)
	}
	store, err := pgstore.New(pgstore.Config{Pool: pool})
	if err != nil {
		log.Fatal(err)
	}
	emb := ollama.NewEmbedder(client)
	pipe, err := rag.New(rag.Config{Store: store},
		rag.WithContentExtractor(extractor.NewAuto()),
		rag.WithEmbedders(embedderregistry.NewTextOnly(embedderregistry.Text(emb))),
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
```
<!-- /fsrc -->

Run the other programs from the same clone using the commands below.

## Concepts in 2 minutes

| Piece | What it is | Guide |
| --- | --- | --- |
| **Agent** | The loop: model turn, tool calls, results, repeat | [agent](agent/README.md) |
| **Adapter** | A `types.Provider` for one serving API: a vendor (Anthropic, OpenAI, Google) or a runtime (Ollama) | [concepts](docs/concepts.md#vendors-runtimes-and-adapters) |
| **Catalog and presets** | What each model accepts, and named chains of complete configurations | [catalog](docs/catalog.md) |
| **Parts** | Messages and model output as ordered typed parts: text, media, tool calls, citations | [parts](docs/parts.md) |
| **Dials** | Model-neutral settings compiled per attempt | [dials](docs/dials.md) |
| **Tools** | Typed Go functions, MCP imports and tool packs; parallelism and tool choice | [tool calling](docs/tool-calling.md), [typed tools](docs/func-tools.md) |
| **Delegation** | Handoffs and sub-agents | [delegation](docs/delegation.md) |
| **Agent definitions** | One agent as a versioned Markdown file the CLI, server and MCP server all load | [agent definitions](docs/agent-definitions.md) |
| **RAG** | Ingest, chunk, embed, hybrid search on Postgres, citations | [rag](rag/README.md) |
| **Evals** | Scorers, gates and experiments for agents, retrieval and graphs | [eval](eval/README.md) |
| **Batches** | Many single-turn calls as one vendor batch job, resumable by job ID | [batch processing](docs/batch.md) |
| **Durable runs** | Journaled steps that resume after a crash, on a local engine or Postgres | [durable execution](docs/durable-execution.md) |

See [concepts](docs/concepts.md) for the full picture.

## Examples

| Example | Run from the repository root |
| --- | --- |
| [Minimal agent](examples/quickstart/agent/main.go) | `go run ./examples/quickstart/agent` |
| [Typed tool](examples/quickstart/tool/main.go) | `go run ./examples/quickstart/tool` |
| [Structured output](examples/quickstart/structured/main.go) | `go run ./examples/quickstart/structured` |
| [Model-neutral dials](examples/quickstart/dials/main.go) | `go run ./examples/quickstart/dials` |
| [Postgres RAG](examples/quickstart/rag/main.go) | `go run ./examples/quickstart/rag` |

The agent examples require a configured model provider. The RAG example requires the Postgres and Ollama setup above. See the [full example index](examples/README.md) for sub-agents, handoffs, durability, caching, approvals and knowledge graphs.

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
