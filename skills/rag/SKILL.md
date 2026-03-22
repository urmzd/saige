---
name: rag
description: Build multi-modal RAG pipelines with graph-enhanced retrieval in Go using saige/rag
version: 0.1.0
author: urmzd
tags: [rag, retrieval, embeddings, knowledge-graph, go, saige]
---

# rag

A Go library for multi-modal Retrieval-Augmented Generation with graph-enhanced retrieval, part of the saige SDK.

## What it does

saige/rag models documents as hierarchical structures (Document -> Section -> ContentVariant) where each section can have multiple modality representations (text, image, table, audio). It provides a pluggable pipeline for ingesting documents, generating embeddings, performing vector search, and optionally extracting entities into a knowledge graph via saige/knowledge.

## When to use

- Ingesting documents (PDF, text, images) into a searchable vector store
- Building RAG pipelines with multi-modal content support
- Combining vector search with knowledge graph traversal
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
    "github.com/urmzd/saige/rag/memstore"
    "github.com/urmzd/saige/rag/types"
)

pipe, err := rag.NewPipeline(
    rag.WithStore(memstore.New()),
    rag.WithContentExtractor(myExtractor),
    rag.WithEmbedders(myEmbedderRegistry),
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
for _, v := range results.Variants {
    fmt.Printf("[%.4f] %s\n", v.Score, v.Variant.Text[:80])
}
```

### With knowledge graph integration

```go
pipe, err := rag.NewPipeline(
    rag.WithStore(store),
    rag.WithContentExtractor(extractor),
    rag.WithGraph(kgGraph),  // enables entity extraction
)
```

## Key interfaces

| Interface | Purpose |
|-----------|---------|
| `Pipeline` | Orchestrate ingest, search, reconstruct |
| `Store` | Document CRUD + vector search |
| `ContentExtractor` | Raw bytes -> structured Document |
| `Chunker` | Split long sections |
| `EmbedderRegistry` | Dispatch embedding by ContentType |

## Configuration options

| Option | Purpose |
|--------|---------|
| `WithStore(s)` | Set the document store (required) |
| `WithContentExtractor(ext)` | Set the content extractor (required) |
| `WithChunker(ch)` | Set the chunker (optional) |
| `WithEmbedders(reg)` | Set the embedder registry (optional, needed for search) |
| `WithGraph(g)` | Enable knowledge graph entity extraction (optional) |
| `WithDedupBehavior(b)` | Set dedup behavior: DedupSkip (default) or DedupReplace |
| `WithStoreOriginals(true)` | Persist raw document bytes |
