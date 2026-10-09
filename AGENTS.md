# saige

A Go SDK for building AI agents, giving them context and memory (RAG, with knowledge graphs as one backend), and evaluating them.

## Architecture

| Package | Role |
|---------|------|
| `cmd/saige/` | CLI: `chat` (interactive TUI), `ask` (single-shot), `rag`/`kg` (standalone ops), `eval`, `serve`, `models`, `update`, `version` |
| `cmd/saige-mcp/` | MCP server binary: exposes tool packs (research, kg) over stdio JSON-RPC |
| `agent/` | Streaming agent loop, tool dispatch, sub-agents, handoffs, durable runs, provider adapters |
| `agent/types/` | Sealed types: Message, Delta, Content, Tool/RichTool, Provider, Cache, StepRunner, FeedbackContent, HandoffContent |
| `agent/tree/` | Conversation tree with branching, compaction, WAL, feedback leaf nodes |
| `agent/provider/` | Ollama, OpenAI, Anthropic, Google adapters; `provider.Build` factory |
| `agent/provider/cache/` | Response-cache decorator: memoizes ChatStream by deterministic request hash |
| `agent/provider/retry/`, `agent/provider/fallback/` | Retry with backoff and Retry-After; ordered fallback across providers |
| `agent/provider/router/` | Routing sessions over complete model configurations: sticky and affinity policies, route locks, classified failover |
| `agent/provider/catalog/` | Model catalog: embedded `data/default.json`, strict loading, layered sources, merge rules, preset resolution and validation |
| `agent/provider/preset/` | Builds a catalog preset into per-entry adapters behind one router, with groups per preset |
| `agent/provider/split/` | Traffic splits: weighted arms, guarded canaries, shadow arms with their own budget |
| `agent/provider/wrapper/` | Decorator conventions: `Unwrap`, `As`, `Members`, `Innermost` |
| `agent/privacy/` | Swaps personal data for placeholders at the provider and tool boundary |
| `agent/workspace/` | Content-addressed scratch artifacts for a run, with tools to write, read and list them |
| `agent/memory/` | Durable, host-scoped memory across conversations; writes need approval |
| `agent/mcp/` | MCP client pool: imports remote tools behind capability gates |
| `agent/otel/` | OpenTelemetry spans and metrics for providers, tools and runs |
| `agent/agui/` | Maps the Delta stream to AG-UI protocol events and writes them as SSE |
| `agent/cache/memcache/` | In-memory LRU `types.Cache[V]` with TTL |
| `agent/notify/` | In-memory `types.Notifier`, subscriber fan-out, `Listen` triggers, and `Cache`, a local level kept coherent through a notifier |
| `agent/durable/dbos/` | DBOS Transact-backed durable `StepRunner` + workflow engine (resumable runs) |
| `agent/tui/` | Bubbletea interactive + verbose streaming TUI |
| `agent/agenttest/` | ScriptedProvider, MockTool for testing |
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
| `tools/research/` | Research tools: web search, file search/read, knowledge graph CRUD |
| `tools/fs/` | Workspace file tools: read (paging), glob, grep, write, edit; root-confined, read-only by default |
| `tools/exec/` | Sandboxed bash tool: Sandbox interface, subprocess backend, command/env/network policy, always approval-marked |
| `tools/fetch/` | URL fetch tool and SafeHTTPClient, which blocks private and metadata IPs |
| `eval/` | Evaluation suites: gates, comparisons and experiments, results store |

## CLI

```bash
saige chat                          # interactive multi-turn TUI
saige chat --provider anthropic     # use Anthropic (needs ANTHROPIC_API_KEY)
saige chat --verbose                # plain-text mode
saige ask "question"                # single-shot query
echo "question" | saige ask --template minimal   # pipe-friendly output
saige rag search --db DSN --query Q # standalone RAG search
saige kg search --db DSN --query Q  # standalone KG search
saige serve --tools fs,fetch --workspace .   # HTTP + SSE turn stream with approve/cancel

# Evals
saige eval init evals                          # scaffold a corpus and manifest
saige eval validate evals                      # check the manifest and corpus offline
saige eval run --manifest evals/saige.eval.json --dry-run
saige eval run --manifest evals/saige.eval.json --concurrency 4 --assert 'aggregate:latency_ms<=5000'
saige eval run --manifest evals/saige.eval.json --store eval-results --resume RUN_ID
saige eval runs --store eval-results           # list recorded runs; show <run-id> for one

# MCP server (separate binary)
saige-mcp --tools research --searxng-url URL  # research tools over MCP/stdio
saige-mcp --tools kg --db DSN                 # KG tools over MCP/stdio
saige-mcp --tools all --db DSN --searxng-url URL
saige-mcp --tools all --read-only             # omit every mutating tool
saige-mcp --tools kg --db DSN --approval elicit   # elicit (default), host, or deny
```

Marked tools ask for approval through the MCP client by default (`--approval elicit`); `host` relies on the client's own permission prompt.
Eval credentials are chosen by the host the request goes to, never by the first variable that happens to be set.

Provider auto-detection: `ANTHROPIC_API_KEY` → `OPENAI_API_KEY` → `GOOGLE_API_KEY` → Ollama.

## Commands

```bash
go test ./...       # run all tests
go vet ./...        # static analysis
go build ./...      # compile all packages
gofmt -w .          # format
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
