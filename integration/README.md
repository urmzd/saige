# Integration tests

End-to-end tests that exercise the SDK against live services. They evaluate
that the whole stack works together, not just each unit in isolation:

| Test | Ollama | Postgres | What it proves |
| --- | :-: | :-: | --- |
| `TestOllamaChatStream` | ✓ | | Streaming chat + usage reporting over the `Provider` contract |
| `TestOllamaStructuredOutput` | ✓ | | Schema-constrained JSON generation |
| `TestOllamaEmbeddings` | ✓ | | Batch embeddings, dimensions, similarity sanity |
| `TestAgentToolCalling` | ✓ | | Full agent loop: LLM tool call → execution → final answer |
| `TestAgentHandoff` | ✓ | | Multi-agent control transfer over a shared tree |
| `TestAgentStructuredResponse` | ✓ | | `WithResponseSchema` on the agent loop |
| `TestAgentPersistencePostgres` | ✓ | ✓ | Tree persisted per turn, rehydrated via `LoadTreeFromStore` |
| `TestRAGPipelinePostgres` | ✓ | ✓ | Ingest → hybrid search (pgvector + BM25) → reconstruct → delete |
| `TestKnowledgeGraphPostgres` | ✓ | ✓ | LLM entity extraction into the graph + semantic fact search |

Tests skip cleanly when their backing service's env var is unset, so plain
`go test ./...` remains hermetic. When a var **is** set but the service is
broken (unreachable host, missing model), tests fail loudly on purpose.

## Quick start

```sh
# 1. Ollama (native install): pull the models once:
ollama pull qwen3.5:4b
ollama pull nomic-embed-text

# 2. Postgres 18 with pgvector and pg_search (ParadeDB image):
just integration-up          # docker compose up, listens on :5433

# 3. Run everything:
just test-integration

# 4. Tear down:
just integration-down
```

Postgres only, no Ollama: `just test-postgres` runs the tests that need just a
database (this package's Postgres-only tests, the `pgstore` packages, the
duraturo durable engine, and the CLI's RAG round trip). CI runs the same set on every pull request against a
ParadeDB (PostgreSQL 18, pgvector, pg_search) service container; tests that also need Ollama skip there.

No native Ollama? Use the containerized one:

```sh
docker compose -f integration/docker-compose.yml --profile ollama up -d
docker compose -f integration/docker-compose.yml exec ollama ollama pull qwen3.5:4b
docker compose -f integration/docker-compose.yml exec ollama ollama pull nomic-embed-text
```

## Environment variables

| Variable | Default (via `just test-integration`) | Purpose |
| --- | --- | --- |
| `SAIGE_TEST_OLLAMA_HOST` | `http://localhost:11434` | Ollama base URL; unset = skip Ollama tests |
| `SAIGE_TEST_OLLAMA_MODEL` | `qwen3.5:4b` | Chat model (needs tool-calling support) |
| `SAIGE_TEST_OLLAMA_EMBED_MODEL` | `nomic-embed-text` | Embedding model (768-dim to match migrations) |
| `SAIGE_TEST_POSTGRES_DSN` | `postgres://postgres:test@localhost:5433/postgres?sslmode=disable` | Postgres/AlloyDB DSN; unset = skip DB tests |

## AlloyDB

The suite runs against AlloyDB only when the instance offers PostgreSQL 18
and both the `vector` and `pg_search` extensions; migrations refuse a server
without them (see [deployment](../docs/deployment.md)). Point the DSN at your
instance (via the AlloyDB Auth Proxy or a private IP):

```sh
# e.g. through the auth proxy listening on localhost:5432
export SAIGE_TEST_POSTGRES_DSN='postgres://USER:PASSWORD@localhost:5432/DATABASE?sslmode=disable'
go test ./integration/ -v -count=1 -timeout 30m
```

Requirements: the role needs `CREATE` on the database (migrations and `CREATE
EXTENSION vector` and `pg_search`).

## Notes and troubleshooting

- **Embedding dimensions.** Migrations create `vector(768)` columns by
  default, matching `nomic-embed-text`. If you switch to a model with a
  different width, the RAG/KG tests skip with instructions: re-run
  migrations on a fresh database with matching `MigrationOptions` dims.
- **Model strength.** Tool calling, handoffs, and entity extraction depend on
  model quality. The defaults are chosen to pass with a small local model; if
  `TestKnowledgeGraphPostgres` or `TestAgentHandoff` flake, try a larger
  model via `SAIGE_TEST_OLLAMA_MODEL`.
- **State.** Tests truncate the tables they own (`rag_*`, `kg_*`) and
  namespace agent conversations by UUID, so reruns against the same database
  are safe. Wipe the compose volume (`just integration-down`) for a fully
  fresh slate.
- **Timeouts.** First runs are slower (model load into memory). The Justfile
  passes `-timeout 30m`; CPU-only machines may need it.
