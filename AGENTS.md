# saige

A unified Go SDK combining AI agent orchestration, knowledge graph construction, and RAG pipelines.

## Architecture

| Package | Role |
|---------|------|
| `agent/` | Streaming agent loop, tool dispatch, sub-agents, provider adapters |
| `agent/types/` | Sealed types: Message, Delta, Content, Tool, Provider interfaces, FeedbackContent, NodeFeedback |
| `agent/tree/` | Conversation tree with branching, compaction, WAL, feedback leaf nodes |
| `agent/provider/` | Ollama, OpenAI, Anthropic, Google adapters |
| `agent/tui/` | Bubbletea interactive + verbose streaming TUI |
| `agent/agenttest/` | ScriptedProvider, MockTool for testing |
| `knowledge/` | Knowledge graph public API (NewGraph, query helpers) |
| `knowledge/types/` | Core knowledge types: Entity, Relation, Fact, Episode, Graph/Store interfaces |
| `knowledge/surrealdb/` | SurrealDB Store implementation |
| `knowledge/internal/` | Engine orchestration, extraction pipeline, fuzzy matching |
| `rag/` | RAG pipeline configuration and constructor |
| `rag/types/` | Core RAG types: Document, Section, Variant, Pipeline/Store interfaces |
| `rag/chunker/` | Recursive and semantic text chunking |
| `rag/bm25retriever/` | In-memory BM25 lexical search |
| `rag/vectorretriever/` | Vector similarity search |
| `rag/graphretriever/` | Knowledge graph-based retrieval |
| `rag/reranker/` | MMR diversity + cross-encoder reranking |
| `rag/rageval/` | Evaluation metrics (precision, recall, faithfulness) |
| `rag/adktool/` | ADK tool bindings for RAG pipeline operations |

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
