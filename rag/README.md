# rag

Multi-modal RAG pipelines: document ingestion with pluggable chunking, multi-retriever fusion, reranking, query transformation, and citation-aware context assembly.

```go
import "github.com/urmzd/saige/rag"
```

Full API reference: [pkg.go.dev/github.com/urmzd/saige/rag](https://pkg.go.dev/github.com/urmzd/saige/rag)

## Contents

- [Quick Start](#quick-start)
- [Data Model](#data-model)
- [Pipeline Interface](#pipeline-interface)
- [Chunking](#chunking)
- [Retrieval](#retrieval)
- [Reranking](#reranking)
- [Context Assembly](#context-assembly)
- [Query Transformation](#query-transformation)
- [Evaluation Metrics](#evaluation-metrics)
- [Agent Tool Bindings](#agent-tool-bindings)
- [SearXNG Client](#searxng-client)

## Quick Start

```go
import (
    "github.com/urmzd/saige/rag"
    "github.com/urmzd/saige/rag/types"
    "github.com/urmzd/saige/rag/pgstore"
    "github.com/urmzd/saige/postgres"
)

pool, _ := postgres.NewPool(ctx, postgres.Config{URL: "postgres://localhost:5432/mydb"})
postgres.RunMigrations(ctx, pool, postgres.MigrationOptions{})

pipe, _ := rag.NewPipeline(
    rag.WithStore(pgstore.NewStore(pool, nil)),
    rag.WithContentExtractor(myExtractor),
    rag.WithEmbedders(myEmbedderRegistry),
    rag.WithRecursiveChunker(512, 50),
    rag.WithBM25(nil),
    rag.WithMMR(0.7),
)
defer pipe.Close(ctx)

pipe.Ingest(ctx, &types.RawDocument{
    SourceURI: "https://example.com/paper.pdf",
    Data:      pdfBytes,
})

result, _ := pipe.Search(ctx, "attention mechanism", types.WithLimit(5))
fmt.Println(result.AssembledContext.Prompt) // context with citations
```

See [`examples/rag/arxiv/`](../examples/rag/arxiv/) for a full pipeline over arXiv papers.

## Data Model

```
Document (fingerprint for dedup, metadata, source URI)
  └── Section[] (ordered by index, optional heading)
        └── ContentVariant[] (text, image, table, audio: each with bytes, embedding, MIME)
```

Every `ContentVariant` has a `.Text` field that is always populated, enabling uniform search and entity extraction.

## Pipeline Interface

```go
type Pipeline interface {
    Ingest(ctx, raw) (*IngestResult, error)
    Search(ctx, query, opts...) (*SearchPipelineResult, error)
    Lookup(ctx, variantUUID) (*SearchHit, error)
    Update(ctx, documentUUID, raw) (*IngestResult, error)
    Delete(ctx, documentUUID) error
    Reconstruct(ctx, documentUUID) (*Document, error)
    Close(ctx) error
}
```

Stores: [`rag/pgstore`](pgstore/) (PostgreSQL + pgvector HNSW) and [`rag/memstore`](memstore/) (in-memory, no external deps).

## Chunking

| Strategy | Description |
|----------|-------------|
| Recursive | Tries separators (`\n\n`, `\n`, `. `, ` `) with configurable overlap |
| Semantic | Splits where embedding similarity drops below threshold |

```go
rag.WithRecursiveChunker(512, 50)       // maxSize, overlap
rag.WithSemanticChunker(0.1, 100, 1000) // threshold, minSize, maxSize
```

## Retrieval

| Retriever | Description |
|-----------|-------------|
| Vector | Embed query, cosine similarity search |
| BM25 | In-memory inverted index with configurable K1/B |
| Graph | Knowledge graph facts resolved to document variants via episode provenance |
| Parent | Wraps any retriever, expands hits to full parent section context |

Multiple retrievers are combined via **Reciprocal Rank Fusion**.

```go
rag.WithBM25(nil)          // default K1=1.2, B=0.75
rag.WithParentContext()    // expand to parent sections
```

## Reranking

| Reranker | Description |
|----------|-------------|
| MMR | Maximal Marginal Relevance: balances relevance and diversity |
| Cross-Encoder | Pair-wise scoring via custom `Scorer` interface |

```go
rag.WithMMR(0.7)               // lambda=0.7
rag.WithCrossEncoder(myScorer) // custom scorer
```

## Context Assembly

Built-in citation support:

```go
// Default: numbered citations with source URIs
// Compressing: LLM-based extraction of relevant sentences
rag.WithCompression(myLLM)
```

## Query Transformation

**HyDE** (Hypothetical Document Embeddings) generates hypothetical documents via LLM for better retrieval:

```go
rag.WithHyDE(myLLM, 3) // generate 3 hypothetical docs
```

## Evaluation Metrics

9 metrics across retrieval, generation, and end-to-end evaluation live in [`rag/eval`](eval/). They are also available as composable `Scorer` adapters for the [universal eval framework](../eval/README.md): see `ContextPrecisionScorer()`, `FaithfulnessScorer()`, and friends.

| Metric | Type | Description |
|--------|------|-------------|
| `ContextPrecision` | Retrieval | Average Precision over relevant UUIDs |
| `ContextRecall` | Retrieval | Fraction of relevant UUIDs in results |
| `NDCG` | Retrieval | Normalized Discounted Cumulative Gain at rank k |
| `MRR` | Retrieval | Reciprocal Rank of first relevant result |
| `HitRate` | Retrieval | Binary: any relevant doc in top-k? |
| `Faithfulness` | Generation | Claim decomposition + verification against context |
| `AnswerRelevancy` | Generation | RAGAS-style synthetic question similarity |
| `AnswerCorrectness` | Generation | LLM-judged comparison to ground truth |
| `LLMJudge` | Generation | Pointwise scoring with custom rubric |

```go
import "github.com/urmzd/saige/rag/eval"

// Retrieval metrics (pure functions, no LLM needed).
precision := eval.ContextPrecision(hits, relevantUUIDs)
recall := eval.ContextRecall(hits, relevantUUIDs)
ndcg := eval.NDCG(hits, relevantUUIDs, 10)
mrr := eval.MRR(hits, relevantUUIDs)
hitRate := eval.HitRate(hits, relevantUUIDs, 10)

// Generation metrics (require LLM and/or embedders).
faith, detail, _ := eval.Faithfulness(ctx, response, contextText, llm)
relevancy, _ := eval.AnswerRelevancy(ctx, query, response, llm, embedders, 3)
correctness, _ := eval.AnswerCorrectness(ctx, response, groundTruth, llm)
score, reason, _ := eval.LLMJudge(ctx, query, response, contextText, rubric, llm)

// Full evaluation pipeline with functional options.
results, _ := eval.Evaluate(ctx, cases, pipeline,
    eval.WithLLM(llm),
    eval.WithEmbedders(embedders),
    eval.WithK(10),
    eval.WithJudgeRubric("Score helpfulness, accuracy, and completeness."),
)
```

## Agent Tool Bindings

The [`rag/tool`](tool/) package exposes the pipeline as agent tools:

```go
import ragtool "github.com/urmzd/saige/rag/tool"

ragTools := ragtool.NewTools(pipeline)
// rag_search, rag_lookup, rag_update, rag_delete, rag_reconstruct
```

## SearXNG Client

The [`rag/source/searxng`](source/searxng/) package provides a standalone HTTP client for SearXNG metasearch instances:

```go
import "github.com/urmzd/saige/rag/source/searxng"

client := searxng.New("http://localhost:8080")
results, _ := client.Search(ctx, "retrieval augmented generation")
// []searxng.Result with Title, URL, Snippet
```

## Related

- [`tools/research`](../tools/research/): web search, file, and knowledge graph tools built on this package
- [`knowledge`](../knowledge/README.md): knowledge graph SDK backing the graph retriever
- [Root README](../README.md): project overview and installation
