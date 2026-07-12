# knowledge

Build and query knowledge graphs with LLM-powered entity extraction, fuzzy deduplication, temporal relation tracking, and hybrid search.

```go
import "github.com/urmzd/saige/knowledge"
```

Full API reference: [pkg.go.dev/github.com/urmzd/saige/knowledge](https://pkg.go.dev/github.com/urmzd/saige/knowledge)

## Quick Start

```go
import (
    "github.com/urmzd/saige/knowledge"
    "github.com/urmzd/saige/knowledge/types"
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

See [`examples/knowledge/basic/`](../examples/knowledge/basic/) for a runnable program.

## Graph Interface

```go
type Graph interface {
    ApplyOntology(ctx, ontology) error
    IngestEpisode(ctx, episode) (*IngestResult, error)
    GetEntity(ctx, uuid) (*Entity, error)
    SearchFacts(ctx, query, opts...) (*SearchFactsResult, error)
    GetGraph(ctx) (*GraphData, error)
    GetNode(ctx, uuid, depth) (*NodeDetail, error)
    GetFactProvenance(ctx, factID) ([]Episode, error)
    Close(ctx) error
}
```

## Core Types

| Type | Purpose |
|------|---------|
| `Entity` | Node: UUID, Name, Type, Summary, Embedding |
| `Relation` | Edge: Source/Target UUID, Type, Fact, ValidAt/InvalidAt |
| `Fact` | Relation with resolved source/target entities |
| `Episode` | Text input with Name, Body, Source, GroupID, Metadata |
| `Ontology` | Schema constraints: EntityTypes, RelationTypes |

## Hybrid Search

Combines vector similarity (HNSW) and full-text (BM25) via **Reciprocal Rank Fusion**:

```go
results, _ := graph.SearchFacts(ctx, "Who works at Acme?",
    types.WithLimit(10),
    types.WithGroupID("project-alpha"),
)
for _, fact := range knowledge.FactsToStrings(results.Facts) {
    fmt.Println(fact) // "Alice -> Acme Corp: works at"
}
```

## Deduplication

- **Exact match** by (name, type) pair
- **Fuzzy match** via Levenshtein distance (threshold 0.8)
- **Relation dedup** by text similarity (threshold 0.92)

## Graph Traversal

```go
detail, _ := graph.GetNode(ctx, entityUUID, 2) // BFS to depth 2
sub := knowledge.Subgraph(detail)              // extract visualization data
```

## Graph Formatting

The [`knowledge/graph`](graph/) package provides DOT and text formatters for visualization:

```go
import "github.com/urmzd/saige/knowledge/graph"

dot := graph.ToDOT(graphData)   // Graphviz DOT
text := graph.ToText(graphData) // human/AI-readable summary
```

## Agent Tool Bindings

The [`knowledge/tool`](tool/) package exposes the graph as agent tools:

```go
import kgtool "github.com/urmzd/saige/knowledge/tool"

kgTools := kgtool.NewTools(graph)
// kg_search, kg_ingest
```

## PostgreSQL Backend

Automatic schema provisioning via `postgres.RunMigrations` with pgvector HNSW index (configurable dimension, cosine distance), tsvector fulltext search, pg_trgm fuzzy matching, unique constraints, and temporal relation tracking. See [`knowledge/pgstore`](pgstore/) for the Store implementation.

## Related

- [`knowledge/eval`](eval/): entity/relation extraction scorers for the [eval framework](../eval/README.md)
- [`rag/graphretriever`](../rag/graphretriever/): use graph facts as a RAG retriever
- [Root README](../README.md): project overview and installation
