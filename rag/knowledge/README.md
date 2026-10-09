# knowledge

Build and query knowledge graphs with LLM-powered entity extraction, fuzzy deduplication, temporal relation tracking, and hybrid search.

```go
import "github.com/urmzd/saige/rag/knowledge"
```

Full API reference: [pkg.go.dev/github.com/urmzd/saige/rag/knowledge](https://pkg.go.dev/github.com/urmzd/saige/rag/knowledge)

## Quick Start

```go
import (
    "github.com/urmzd/saige/rag/knowledge"
    "github.com/urmzd/saige/rag/knowledge/types"
    "github.com/urmzd/saige/postgres"
    "github.com/urmzd/saige/agent/provider/ollama"
)

// Connect to PostgreSQL (requires pgvector extension).
pool, _ := postgres.NewPool(ctx, postgres.Config{URL: "postgres://localhost:5432/mydb"})
postgres.RunMigrations(ctx, pool, postgres.MigrationOptions{})

client := ollama.NewClient("http://localhost:11434", "qwen2.5", "nomic-embed-text")
graph, _ := knowledge.NewGraph(ctx,
    knowledge.WithPostgres(pool),
    knowledge.WithExtractor(knowledge.NewOllamaExtractor(client)),
    knowledge.WithEmbedder(knowledge.NewOllamaEmbedder(client)),
)
defer graph.Close(ctx)

graph.IngestEpisode(ctx, &types.EpisodeInput{
    Name: "meeting-notes",
    Body: "Alice presented the Q4 roadmap. Bob raised concerns about the timeline.",
})

results, _ := graph.SearchFacts(ctx, "Who presented the roadmap?")
```

See [`examples/knowledge/basic/`](../../examples/knowledge/basic/) for a runnable program.

## Graph Interface

```go
type Graph interface {
    ApplyOntology(ctx context.Context, ont *Ontology) error
    IngestEpisode(ctx context.Context, input *EpisodeInput) (*IngestResult, error)
    GetEntity(ctx context.Context, id string) (*Entity, error)
    SearchFacts(ctx context.Context, query string, opts ...SearchOption) (*SearchFactsResult, error)
    GetGraph(ctx context.Context, limit int64) (*GraphData, error)
    GetNode(ctx context.Context, id string, depth int) (*NodeDetail, error)
    GetFactProvenance(ctx context.Context, factUUID string) ([]Episode, error)
    Close(ctx context.Context) error
}
```

## Core Types

| Type | Purpose |
|------|---------|
| `Entity` | Node: UUID, Name, Type, Summary |
| `Relation` | Edge: Source/Target UUID, Type, Fact, ValidAt/InvalidAt |
| `Fact` | Relation with resolved source/target entities |
| `EpisodeInput` | Text to ingest: Name, Body, Source, GroupID, DocumentID, ReferenceTime, Metadata |
| `Ontology` | Entity and relation types passed to the extractor |

`GroupID` is the tenant scope: entities are deduplicated within a group. `DocumentID` names the source document, so `DeleteDocumentEpisodes` can remove one document's episodes without touching the rest of the group. A fact that the removed document had superseded becomes current again unless a remaining fact supersedes it.

## Ingest

`IngestEpisode` fails when extraction fails or the episode cannot be stored. Failures of single entities, relations, embeddings, or links return the stored parts with an error wrapping `types.ErrPartialEpisode`:

```go
res, err := graph.IngestEpisode(ctx, input)
if err != nil && !errors.Is(err, types.ErrPartialEpisode) {
    return err
}
```

All entities of an episode are embedded in one `Embed` call. With the Postgres store, the episode is stored first and linked to every relation it asserts, including relations it repeats. `GetFactProvenance` returns those episodes, oldest first.

## Ontology

`ApplyOntology` passes the ontology to an extractor that implements `types.OntologyExtractor` (the Ollama extractor does). Extracted types that match an ontology type ignoring case and punctuation are rewritten to the ontology's spelling, so `"person"` is stored as `"Person"`. Other types are kept unless the graph is built with `knowledge.WithStrictOntology()`, which drops them.

## Hybrid Search

Combines vector similarity (HNSW) and Postgres full-text search (`ts_rank`) via **Reciprocal Rank Fusion**. Text search matches entity names and summaries and the fact text and relation type, so "who reports to Bob" finds a `reports_to` fact:

```go
results, _ := graph.SearchFacts(ctx, "Who works at Acme?",
    types.WithLimit(10),
    types.WithGroupID("project-alpha"),
)
for _, fact := range knowledge.FactsToStrings(results.Facts) {
    fmt.Println(fact) // "Alice -> Acme Corp: works at"
}
```

The limit is applied in SQL. Each matched entity contributes at most `Limit` of its newest edges, and ties are ordered deterministically.

A search that succeeds on one backend and fails on the other returns its results with `types.ErrPartialSearch`, the same sentinel as the RAG pipeline's, so one `errors.Is` check covers both.

`knowledge.WithObserver(obs)` reports episode ingest, fact search, and deletion as spans (`knowledge.ingest_episode`, `knowledge.search_facts`, `knowledge.delete`) and each fact search as a retrieval metric. `rag/otel` adapts OpenTelemetry to the observer; see the [rag README](../README.md#observability).

## Temporal Facts

Relations become valid at `EpisodeInput.ReferenceTime` (default: ingest time). A new relation of the same source, target, type, and direction invalidates an older active one at its own valid time. A backfilled relation older than the stored one is created already invalidated, so it never looks current. A reversed edge ("B reports_to A") neither duplicates nor supersedes "A reports_to B".

`types.WithValidAt(t)` runs an as-of query: it returns the relations valid at `t`, including ones superseded since.

## Deduplication

- **Exact match** by (name, type) pair
- **Fuzzy match** via Levenshtein distance (threshold 0.8); a match keeps the existing entity's name and type
- **Relation dedup** by fact text similarity (threshold 0.92), same direction only

## Graph Traversal

```go
detail, _ := graph.GetNode(ctx, entityUUID, 2) // BFS to depth 2
sub := knowledge.Subgraph(detail)              // extract visualization data
```

The Postgres store runs one query per hop and returns each edge once. It stops at 1000 nodes or 5000 edges by default and sets `NodeDetail.Truncated`; change the caps with `pgstore.NewStore(pool, logger, pgstore.WithTraversalLimits(nodes, edges))`.

## Graph Formatting

The [`rag/knowledge/graph`](graph/) package provides DOT and text formatters for visualization:

```go
import "github.com/urmzd/saige/rag/knowledge/graph"

dot := graph.ToDOT(graphData)   // Graphviz DOT
text := graph.ToText(graphData) // human/AI-readable summary
```

## Agent Tool Bindings

The [`rag/knowledge/tool`](tool/) package exposes the graph as agent tools:

```go
import kgtool "github.com/urmzd/saige/rag/knowledge/tool"

kgTools := kgtool.NewTools(graph, kgtool.WithGroupID(tenantID))
// kg_search, kg_ingest
```

`WithGroupID` binds both tools to one group: `kg_search` searches only it and `kg_ingest` writes into it. The model cannot choose the group. `kg_search` caps the model's limit at 50.

## PostgreSQL Backend

Automatic schema provisioning via `postgres.RunMigrations` with pgvector HNSW index (configurable dimension, cosine distance), tsvector full-text search, pg_trgm fuzzy matching, unique constraints, relation-to-episode links, and temporal relation tracking. See [`rag/knowledge/pgstore`](pgstore/) for the Store implementation.

## Related

- [`rag/knowledge/eval`](eval/): entity/relation extraction scorers for the [eval framework](../../eval/README.md)
- [`rag/graphretriever`](../graphretriever/): use graph facts as a RAG retriever
- [Root README](../../README.md): project overview and installation
