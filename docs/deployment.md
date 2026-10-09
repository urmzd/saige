# Deployment

## Minimal deployment

saige needs one database and nothing else: a single PostgreSQL 18 server with two extensions.

| Component | Role |
| --- | --- |
| PostgreSQL 18 or later | Conversation trees, checkpoints, RAG documents, and the knowledge graph. `postgres.RunMigrations` refuses older servers. |
| [pgvector](https://github.com/pgvector/pgvector) (`vector`) | Embedding columns and HNSW vector search. |
| [ParadeDB pg_search](https://github.com/paradedb/paradedb) (`pg_search`) | The BM25 index on `rag_variant.text`, used by `rag/pgstore` keyword search. |
| LISTEN/NOTIFY | Built into PostgreSQL. Cross-process notifications use it, so no separate message broker is needed. |

There is no Redis, no separate search engine, and no in-process index to rebuild. Every saige process connected to the database sees the same vectors, the same BM25 index, and the same notifications.

The `paradedb/paradedb` image ships PostgreSQL 18 with both extensions. CI and the integration compose file pin this tag:

```sh
docker run -d --name saige-postgres -p 5432:5432 \
  -e POSTGRES_PASSWORD=postgres \
  paradedb/paradedb:0.26.1-pg18
```

Then run migrations once per release, before the new version serves traffic:

```go
pool, err := postgres.NewPool(ctx, postgres.Config{URL: dsn})
if err != nil {
    return err
}
if err := postgres.RunMigrations(ctx, pool, postgres.MigrationOptions{}); err != nil {
    return err // ErrUnsupportedServer or ErrExtensionUnavailable name the missing piece
}
```

`RunMigrations` checks `server_version_num` (180000 or higher) and runs `CREATE EXTENSION IF NOT EXISTS` for `vector` and `pg_search`. The role needs permission to create both extensions, or a superuser must create them first.

### Hybrid search on Postgres

A pipeline over `rag/pgstore` built with `rag.WithBM25` searches through Postgres: `pgstore.Store` implements `types.KeywordSearcher`, so the BM25 arm runs `@@@` with `pdb.score` against the index, with the same scope, time-range, content-type, and metadata filters as vector search. The pipeline fuses both arms with its fuser (RRF by default). To compose retrievers by hand, use `pgstore.NewKeywordRetriever(store)` next to a vector retriever.

The BM25 index lives with the rows it indexes. Inserts, `ReplaceDocument`, and `DeleteDocument` update it in the same transaction, so `rag.RebuildIndex` is not needed. The in-memory `rag/bm25retriever` remains for `rag/memstore`.

### Managed Postgres

A managed service works when it offers PostgreSQL 18 and lets you install both `vector` and `pg_search`. Services that offer pgvector but not pg_search cannot run the migrations.

## Licensing

pg_search is licensed under AGPL-3.0, separately from saige. It runs inside the PostgreSQL server as an extension; saige talks to it over the ordinary Postgres protocol and does not link or redistribute it. Review the AGPL-3.0 terms for how you deploy and modify the database server, and see ParadeDB for commercial licensing.
