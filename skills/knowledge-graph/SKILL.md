---
name: knowledge-graph
description: Build and query knowledge graphs with PostgreSQL + pgvector — ingest episodes, extract entities/relations via LLM, and search facts by semantic similarity or keyword. Use when working with knowledge graphs, entity extraction, or graph storage.
metadata:
  argument-hint: [query]
---

# knowledge-graph

Build and query knowledge graphs using `saige/knowledge`.

## Quick Start

```go
import (
    "github.com/urmzd/saige/knowledge"
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

// Ingest
graph.IngestEpisode(ctx, &knowledge.EpisodeInput{
    Name: "notes", Body: "Alice presented the roadmap.", Source: "meeting",
})

// Search
facts, _ := graph.SearchFacts(ctx, "roadmap")
```

## Key Operations

| Method | Purpose |
|--------|---------|
| `IngestEpisode` | Extract entities/relations from text and store them |
| `SearchFacts` | Full-text search on relation facts |
| `GetEntity` | Retrieve a single entity by ID |
| `GetNode` | Get a node with its neighborhood (depth N) |
| `GetGraph` | Full graph snapshot for visualization |
| `ApplyOntology` | Constrain entity/relation types |

## Ontology

```go
graph.ApplyOntology(ctx, &knowledge.Ontology{
    EntityTypes:   []knowledge.EntityTypeDef{{Name: "Person", Description: "A human"}},
    RelationTypes: []knowledge.RelationTypeDef{{Name: "works_on", SourceType: "Person", TargetType: "Project"}},
})
```

## Agent Tool Bindings

```go
import "github.com/urmzd/saige/knowledge/tool"

tools := tool.NewTools(graph)
// kg_search, kg_ingest
```

## CLI

```bash
saige kg search --db "$SAIGE_KG_DB" --query "Who presented?"
saige kg ingest --db "$SAIGE_KG_DB" --name "meeting" --text "Alice presented the roadmap."
saige kg graph  --db "$SAIGE_KG_DB"
saige kg node   --db "$SAIGE_KG_DB" --id <entity-uuid> --depth 2
```
