# saige

A unified Go SDK combining AI agent orchestration, knowledge graph construction, and RAG pipelines.

## Architecture

| Package | Role |
|---------|------|
| `cmd/saige/` | CLI: `chat` (interactive TUI), `ask` (single-shot), `rag`/`kg` (standalone ops) |
| `cmd/saige-mcp/` | MCP server binary: exposes tool packs (research, kg) over stdio JSON-RPC |
| `agent/` | Streaming agent loop, tool dispatch, sub-agents, provider adapters |
| `agent/types/` | Sealed types: Message, Delta, Content, Tool, Provider interfaces, FeedbackContent, NodeFeedback |
| `agent/tree/` | Conversation tree with branching, compaction, WAL, feedback leaf nodes |
| `agent/provider/` | Ollama, OpenAI, Anthropic, Google adapters |
| `agent/tui/` | Bubbletea interactive + verbose streaming TUI |
| `agent/agenttest/` | ScriptedProvider, MockTool for testing |
| `knowledge/` | Knowledge graph public API (NewGraph, query helpers) |
| `knowledge/types/` | Core knowledge types: Entity, Relation, Fact, Episode, Graph/Store interfaces |
| `knowledge/pgstore/` | PostgreSQL + pgvector Store implementation (HNSW, tsvector, pg_trgm) |
| `knowledge/tool/` | Agent tool bindings for KG operations (kg_search, kg_ingest) |
| `knowledge/graph/` | Graph formatting utilities (DOT, text) for visualization |
| `knowledge/internal/` | Engine orchestration, extraction pipeline, fuzzy matching |
| `postgres/` | Shared PostgreSQL connection pool and schema migrations |
| `rag/` | RAG pipeline configuration and constructor |
| `rag/types/` | Core RAG types: Document, Section, Variant, Pipeline/Store interfaces |
| `rag/pgstore/` | PostgreSQL + pgvector RAG Store implementation (HNSW vector search) |
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

## CLI

```bash
saige chat                          # interactive multi-turn TUI
saige chat --provider anthropic     # use Anthropic (needs ANTHROPIC_API_KEY)
saige chat --verbose                # plain-text mode
saige ask "question"                # single-shot query
echo "question" | saige ask --raw   # pipe-friendly raw output
saige rag search --db DSN --query Q # standalone RAG search
saige kg search --db DSN --query Q  # standalone KG search

# MCP server (separate binary)
saige-mcp --tools research --searxng-url URL  # research tools over MCP/stdio
saige-mcp --tools kg --db DSN                 # KG tools over MCP/stdio
saige-mcp --tools all --db DSN --searxng-url URL
```

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
