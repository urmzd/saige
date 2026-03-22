---
name: knowledge-graph
description: Build and query knowledge graphs with SurrealDB — ingest episodes, extract entities/relations via LLM, and search facts by semantic similarity or keyword. Use when working with knowledge graphs, entity extraction, or SurrealDB graph storage.
argument-hint: [query]
---

# knowledge-graph

Build and query knowledge graphs using `saige/knowledge`.

## Quick Start

```go
import (
    "github.com/urmzd/saige/knowledge"
    "github.com/urmzd/saige/agent/provider/ollama"
)

// Connect
client := ollama.NewClient("http://localhost:11434", "qwen2.5", "nomic-embed-text")
graph, _ := knowledge.NewGraph(ctx,
    knowledge.WithSurrealDB("ws://localhost:8000", "default", "knowledge", "root", "root"),
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
