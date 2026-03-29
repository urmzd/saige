---
name: rag
description: Build multi-modal RAG pipelines with graph-enhanced retrieval in Go using saige/rag — chunking, multi-retriever fusion, reranking, HyDE, evaluation metrics, and PostgreSQL + pgvector storage.
metadata:
  version: 0.1.0
  author: urmzd
  tags: rag retrieval embeddings knowledge-graph go saige
---

# rag

A Go library for multi-modal Retrieval-Augmented Generation with graph-enhanced retrieval, part of the saige SDK.

## What it does

saige/rag models documents as hierarchical structures (Document -> Section -> ContentVariant) where each section can have multiple modality representations (text, image, table, audio). It provides a pluggable pipeline for ingesting documents, generating embeddings, performing hybrid search (vector + BM25 + graph), reranking, and assembling context with citations.

## When to use

- Ingesting documents (PDF, text, images) into a searchable vector store
- Building RAG pipelines with multi-modal content support
- Combining vector search with knowledge graph traversal
- Evaluating retrieval and generation quality
- Deduplicating documents by content fingerprint

## Usage

### Install

```bash
go get github.com/urmzd/saige
```

### Create a pipeline

```go
import (
    "github.com/urmzd/saige/rag"
    "github.com/urmzd/saige/rag/pgstore"
    "github.com/urmzd/saige/rag/types"
    "github.com/urmzd/saige/postgres"
)

// Connect to PostgreSQL (requires pgvector extension).
pool, _ := postgres.NewPool(ctx, postgres.Config{URL: "postgres://localhost:5432/mydb"})
postgres.RunMigrations(ctx, pool, postgres.MigrationOptions{})

pipe, err := rag.NewPipeline(
    rag.WithStore(pgstore.NewStore(pool, nil)),
    rag.WithContentExtractor(myExtractor),
    rag.WithEmbedders(myEmbedderRegistry),
    rag.WithRecursiveChunker(512, 50),
    rag.WithBM25(nil),
    rag.WithMMR(0.7),
)
```

### Ingest a document

```go
result, err := pipe.Ingest(ctx, &types.RawDocument{
    SourceURI: "https://example.com/paper.pdf",
    MIMEType:  "application/pdf",
    Data:      pdfBytes,
})
```

### Search

```go
results, err := pipe.Search(ctx, "attention mechanism", types.WithLimit(5))
fmt.Println(results.AssembledContext.Prompt) // context with citations
```

### With knowledge graph integration

```go
pipe, err := rag.NewPipeline(
    rag.WithStore(store),
    rag.WithContentExtractor(extractor),
    rag.WithGraph(kgGraph),  // enables entity extraction + graph retrieval
)
```

## Key interfaces

| Interface | Purpose |
|-----------|---------|
| `Pipeline` | Orchestrate ingest, search, update, delete, reconstruct |
| `Store` | Document CRUD + vector search |
| `ContentExtractor` | Raw bytes -> structured Document |
| `Chunker` | Split long sections |
| `EmbedderRegistry` | Dispatch embedding by ContentType |

## Configuration options

| Option | Purpose |
|--------|---------|
| `WithStore(s)` | Set the document store (required) |
| `WithContentExtractor(ext)` | Set the content extractor (required) |
| `WithRecursiveChunker(max, overlap)` | Recursive text chunking |
| `WithSemanticChunker(thresh, min, max)` | Semantic similarity chunking |
| `WithEmbedders(reg)` | Set the embedder registry (optional, needed for search) |
| `WithGraph(g)` | Enable knowledge graph entity extraction (optional) |
| `WithBM25(cfg)` | Enable BM25 lexical retrieval |
| `WithParentContext()` | Expand hits to parent section context |
| `WithMMR(lambda)` | MMR diversity reranking |
| `WithCrossEncoder(scorer)` | Cross-encoder reranking |
| `WithHyDE(llm, n)` | HyDE query expansion |
| `WithCompression(llm)` | LLM-based context compression |
| `WithDedupBehavior(b)` | Set dedup behavior: DedupSkip (default) or DedupReplace |
| `WithStoreOriginals(true)` | Persist raw document bytes |

## Evaluation

```go
import "github.com/urmzd/saige/rag/eval"

// Retrieval metrics (pure functions).
precision := eval.ContextPrecision(hits, relevantUUIDs)
recall := eval.ContextRecall(hits, relevantUUIDs)
ndcg := eval.NDCG(hits, relevantUUIDs, 10)
mrr := eval.MRR(hits, relevantUUIDs)

// Generation metrics (require LLM).
faith, _, _ := eval.Faithfulness(ctx, response, contextText, llm)
correctness, _ := eval.AnswerCorrectness(ctx, response, groundTruth, llm)
```

## Agent tool bindings

```go
import "github.com/urmzd/saige/rag/tool"

tools := tool.NewTools(pipeline)
// rag_search, rag_lookup, rag_update, rag_delete, rag_reconstruct
```

## CLI

```bash
saige rag ingest --db "$SAIGE_RAG_DB" --file paper.pdf --mime application/pdf
saige rag search --db "$SAIGE_RAG_DB" --query "attention mechanism"
saige rag lookup --db "$SAIGE_RAG_DB" --uuid <variant-uuid>
saige rag delete --db "$SAIGE_RAG_DB" --uuid <doc-uuid>
```
