# saige

A Go SDK for building AI agents, giving them context and memory (RAG, with knowledge graphs as one backend), and evaluating them.

## Architecture

| Package | Role |
|---------|------|
| `cmd/saige/` | CLI: `chat` (interactive TUI), `ask` (single-shot), `rag`/`kg` (standalone ops), `eval`, `serve`, `acp` (ACP agent over stdio), `export`/`launch` (harness setup), `approvals` (held approvals), `agent` (definitions), `models`, `update`, `version` |
| `cmd/saige-mcp/` | MCP server binary: exposes tool packs (research, kg) and an agent definition as a tool, holding approvals for clients without elicitation |
| `cmd/internal/agenthost/` | Session layer shared by serve, acp and saige-mcp: pinned binding, one turn at a time, grant checks |
| `cmd/internal/approvals/` | File store of held approvals that `saige approvals` decides |
| `agent/` | Streaming agent loop, tool dispatch, sub-agents, handoffs, durable runs, provider adapters |
| `agent/types/` | Sealed types: Message, Delta, Content, Tool/RichTool, Provider, Cache, StepRunner, FeedbackPart, HandoffPart |
| `agent/tree/` | Conversation tree with branching, compaction, WAL, feedback leaf nodes |
| `agent/provider/` | Adapters implementing `types.Provider`: Anthropic (Messages), OpenAI (Chat Completions, Responses), Google (Gemini API, Vertex AI) and the local Ollama runtime; `provider.Build` factory |
| `agent/provider/cache/` | Response-cache decorator: memoizes Stream by deterministic request hash |
| `agent/provider/retry/`, `agent/provider/fallback/` | Retry with backoff and Retry-After; ordered fallback across adapters |
| `agent/provider/router/` | Routing sessions over complete model configurations: sticky and affinity policies, route locks, classified failover |
| `agent/provider/catalog/` | Model catalog: embedded `data/default.json`, strict loading, layered sources, merge rules, preset resolution and validation |
| `agent/provider/preset/` | Builds a catalog preset into per-entry adapters behind one router, with groups per preset |
| `agent/provider/split/` | Traffic splits: weighted arms, guarded canaries, shadow arms with their own budget |
| `agent/provider/wrapper/` | Decorator conventions: `Unwrap`, `As`, `Members`, `Innermost` |
| `agent/privacy/` | Swaps personal data for placeholders at the provider and tool boundary |
| `agent/guardrail/` | Built-in input and output guardrails: PII and regex detection, length, JSON schema, model classifier |
| `agent/workspace/` | Content-addressed scratch artifacts for a run, with tools to write, read and list them |
| `agent/definition/` | Agent definitions: Markdown with YAML frontmatter, strict decoding, JSON Schema, sources (directory, fs.FS, reader, HTTPS, layered, untrusted), registry with `name@range` resolution, digests, pinning and reload, approval rule syntax |
| `agent/definition/bind/` | Binds a resolved definition to agent options: model, harness, MCP and registry tools, skills, memory, sub-agents, approval gate, compaction, guardrails, limits |
| `agent/definition/pgsource/` | Postgres definition source with LISTEN/NOTIFY reload |
| `agent/memory/` | Durable, host-scoped memory across conversations; writes need approval |
| `agent/memory/pgstore/` | Postgres memory store: hybrid pgvector and BM25 recall, retention, and opt-in conversation recall |
| `agent/memory/memorytest/` | Conformance suite every memory store runs |
| `agent/mcp/` | MCP client pool: imports remote tools behind capability gates |
| `agent/otel/` | OpenTelemetry spans and metrics for providers, tools and runs |
| `agent/agui/` | Maps the Delta stream to AG-UI protocol events and writes them as SSE |
| `agent/cache/memcache/` | In-memory LRU `types.Cache[V]` with TTL |
| `agent/notify/` | In-memory `types.Notifier`, subscriber fan-out, `Listen` triggers, and `Cache`, a local level kept coherent through a notifier |
| `agent/batch/` | Batch jobs over `types.BatchProvider`: durable runner and job stores, local fallback, coalescer for evals |
| `agent/durable/local/` | Local durable engine: resumable runs, saved approvals and reconciliation on one machine |
| `agent/durable/duraturo/` | duraturo-backed durable engine: runs on any duraturo ledger and queue, such as Postgres tables |
| `agent/tui/` | Bubbletea interactive + verbose streaming TUI |
| `agent/agenttest/` | Test models (ScriptedProvider, FunctionModel, the tool-call emulator) and MockTool |
| `rag/knowledge/` | Knowledge graph public API (NewGraph, query helpers) |
| `rag/knowledge/types/` | Core knowledge types: Entity, Relation, Fact, Episode, Graph/Store interfaces |
| `rag/knowledge/pgstore/` | PostgreSQL + pgvector Store implementation (HNSW, tsvector, pg_trgm) |
| `rag/knowledge/tool/` | Agent tool bindings for KG operations (kg_search, kg_ingest) |
| `rag/knowledge/graph/` | Graph formatting utilities (DOT, text) for visualization |
| `rag/knowledge/internal/` | Engine orchestration, extraction pipeline, fuzzy matching |
| `postgres/` | Shared PostgreSQL connection pool, schema migrations, `Notifier` (LISTEN/NOTIFY) and `CacheStore` (UNLOGGED cache table) |
| `rag/` | RAG pipeline configuration and constructor |
| `rag/fusion/` | Rank fusion strategies: RRF, Weighted |
| `rag/otel/` | OpenTelemetry adapter for the rag Observer |
| `rag/types/` | Core RAG types: Document, Section, Variant, Pipeline/Store interfaces |
| `rag/pgstore/` | PostgreSQL 18 RAG Store implementation (pgvector HNSW vector search, pg_search BM25 keyword search) |
| `rag/memstore/` | In-memory RAG Store (for testing, no external deps) |
| `rag/chunker/` | Recursive and semantic text chunking |
| `rag/bm25retriever/` | In-memory BM25 lexical search |
| `rag/vectorretriever/` | Vector similarity search |
| `rag/graphretriever/` | Knowledge graph-based retrieval |
| `rag/parentretriever/` | Parent context expansion retriever |
| `rag/reranker/` | MMR diversity + cross-encoder reranking |
| `rag/hyde/` | HyDE (Hypothetical Document Embeddings) query expansion |
| `rag/contextassembler/` | Citation assembly + LLM-based context compression |
| `rag/eval/` | Evaluation metrics (precision, recall, NDCG, MRR, HitRate, faithfulness, relevancy, correctness, LLM-as-judge) |
| `rag/tool/` | Agent tool bindings for RAG pipeline operations |
| `rag/embedderregistry/` | Dispatch embedding by content type |
| `rag/embeddingcache/` | Caching layer for embeddings |
| `rag/extractor/` | Content extraction from raw documents |
| `rag/source/` | Source URI resolution |
| `rag/source/searxng/` | SearXNG metasearch HTTP client |
| `rag/tokenizer/` | Token counting utilities |
| `tools/` | `Harness`: curated toolset over the packs below plus scratch tools, with groups, sandbox choice, approvals and spill |
| `tools/research/` | Research tools: web search, file search/read, knowledge graph CRUD |
| `tools/fs/` | Workspace file tools: read (paging), list, glob, grep, write, edit; root-confined, read-only by default |
| `tools/exec/` | Sandboxed bash and execute_code tools: Sandbox interface, subprocess and Docker backends, command/env/network policy, always approval-marked |
| `tools/fetch/` | URL fetch tool and SafeHTTPClient, which blocks private and metadata IPs |
| `eval/` | Evaluation suites: gates, comparisons and experiments, results store |

## CLI

```bash
saige chat                          # interactive multi-turn TUI
saige chat --provider anthropic     # use Anthropic (needs ANTHROPIC_API_KEY)
saige chat --tools harness          # built-in tools (default readonly); write and exec ask first
saige chat --verbose                # plain-text mode
saige ask "question"                # single-shot query
echo "question" | saige ask --template minimal   # pipe-friendly output
saige rag search --db DSN --query Q # standalone RAG search
saige kg search --db DSN --query Q  # standalone KG search
saige serve --tools fs,fetch --workspace .   # HTTP + SSE turn stream with approve/cancel

# Agent definitions (*.agent.md in ~/.config/saige/agents, .saige/agents, --agents-dir)
saige agent list                               # every definition, highest version first
saige agent show repo-steward@^1               # digests, sub-agents, skill hashes, the file
saige agent validate examples/agents           # offline checks; exit 2 when invalid
saige ask --agents-dir examples/agents --agent assistant "question"   # also chat and serve
saige acp --agents-dir examples/agents --agent assistant             # ACP agent over stdio for editors
saige export claude --agents-dir examples/agents --agent assistant --dry-run   # also codex, gemini, opencode, cursor, skills
saige launch claude --agents-dir examples/agents --agent assistant -- -p "question"
saige approvals list                          # held approvals; approve or deny TOKEN

# Evals
saige eval init evals                          # scaffold a corpus and manifest
saige eval validate evals                      # check the manifest and corpus offline
saige eval run --manifest evals/saige.eval.json --dry-run
saige eval run --manifest evals/saige.eval.json --concurrency 4 --assert 'aggregate:latency_ms<=5000'
saige eval run --manifest evals/saige.eval.json --store eval-results --resume RUN_ID
saige eval runs --store eval-results           # list recorded runs; show <run-id> for one
saige eval run --manifest evals/saige.eval.json --provider anthropic --batch --batch-store .saige/batches
saige eval compare RUN_ID --store eval-results # regression gate vs the previous run: exit 1 regressed, 3 inconclusive

# MCP server (separate binary)
saige-mcp --tools research --searxng-url URL  # research tools over MCP/stdio
saige-mcp --tools kg --db DSN                 # KG tools over MCP/stdio
saige-mcp --tools all --db DSN --searxng-url URL
saige-mcp --tools all --read-only             # omit every mutating tool
saige-mcp --tools kg --db DSN --approval elicit   # elicit (default), host, or deny
saige-mcp --tools kg --db DSN --agents-dir agents --agent researcher   # a definition as the agent tool
```

Marked tools ask for approval through the MCP client by default (`--approval elicit`); `host` relies on the client's own permission prompt.
Eval credentials are chosen by the host the request goes to, never by the first variable that happens to be set.

Default model selection: the first entry of the catalog's `default_preset` with credentials: `ANTHROPIC_API_KEY` → `OPENAI_API_KEY` → Google (Vertex AI env or `GOOGLE_API_KEY`) → a local Ollama server. `--provider` names an adapter; `ollama` is a local runtime, not a vendor (see docs/concepts.md).

## Commands

```bash
go test ./...       # run all tests
go vet ./...        # static analysis
go build ./...      # compile all packages
gofmt -w .          # format
fsrc run README.md  # re-embed examples/quickstart into the README (CI fails on drift)
fsrc run docs/agent-definitions.md  # re-embed examples/agents (a test fails on drift)
```

## Commit Convention

Angular conventional commits enforced by gitit:

- `feat:` new feature
- `fix:` bug fix
- `docs:` documentation
- `refactor:` code restructuring
- `test:` test changes
- `chore:` maintenance
- `ci:` CI/CD changes
- `perf:` performance

## Code Style

- Functional options pattern for configuration
- Interface-first design with pluggable implementations
- Sealed interfaces via unexported marker methods
- Table-driven tests with comprehensive mocks
- `slog.Logger` for structured logging
- No abbreviations except established ones (KG, RAG, BM25, RRF, MMR)
