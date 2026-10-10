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
- [Scopes](#scopes)
- [Source Sync](#source-sync)
- [Chunking](#chunking)
- [Retrieval](#retrieval)
- [Reranking](#reranking)
- [Context Assembly](#context-assembly)
- [Query Transformation](#query-transformation)
- [Observability](#observability)
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
    rag.WithBM25(nil), // BM25 runs in Postgres through pg_search
    rag.WithMMR(0.7),
)
defer pipe.Close(ctx)

pipe.Ingest(ctx, &types.RawDocument{
    SourceURI: "https://example.com/paper.pdf",
    Data:      pdfBytes,
})

result, _ := pipe.Search(ctx, "attention mechanism", types.WithLimit(5), types.WithContextAssembly(4096))
fmt.Println(result.Context.Prompt) // context with citations
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

Stores: [`rag/pgstore`](pgstore/) (PostgreSQL 18 + pgvector HNSW + pg_search BM25; see [deployment](../docs/deployment.md)) and [`rag/memstore`](memstore/) (in-memory, no external deps).

Ingest behavior:

- Extract, chunk, and embed run before any store write, so a failure leaves the store unchanged.
- `Update` keeps the document UUID and swaps the content atomically when the store implements `DocumentReplacer`. A failed update leaves the old version, its BM25 entries, and its graph episodes in place.
- Text is cleaned before it is stored: NUL bytes are removed and invalid UTF-8 becomes U+FFFD, because PostgreSQL `TEXT` and `JSONB` reject both.
- Concurrent ingests of identical content resolve to one document. The others return it with `Deduplicated` set.
- A failure after the commit (original bytes, retriever index, or knowledge graph) returns the result together with an error wrapping `ErrPartialIngest`.
- An embedder that returns the wrong number of vectors or an empty vector fails the ingest with `ErrEmbeddingShape`.

Filtered vector search on pgstore uses pgvector iterative index scans (pgvector 0.8.0 or later) so a selective filter still fills the limit:

```go
pgstore.NewStore(pool, nil,
    pgstore.WithIterativeScan(pgstore.IterativeScanStrict), // default: auto-detect
    pgstore.WithEFSearch(100),
)
```

## Scopes

A scope is an isolation boundary such as a tenant. Every document belongs to one scope, and the empty string is the default scope. Stores apply the scope as an exact-match predicate before the limit, so another scope's nearer vectors never use up a page, and deduplication only matches documents of the same scope: identical bytes ingested by two tenants become two documents.

```go
// One pipeline per tenant: ingests, searches, and UUID lookups stay inside "acme".
pipe, _ := rag.NewPipeline(rag.WithStore(store), rag.WithContentExtractor(ext), rag.WithScope("acme"))

// One shared pipeline: each call names its scope.
pipe.Ingest(ctx, &types.RawDocument{SourceURI: uri, Data: data, Scope: "acme"})
pipe.Search(ctx, "quarterly revenue", types.WithScope("acme"))
```

A pipeline built with `rag.WithScope` rejects a request naming another scope with `types.ErrScopeMismatch`, and `Lookup`, `Update`, `Delete`, and `Reconstruct` treat documents of other scopes as missing. When no graph namespace is set, the scope is also the knowledge-graph namespace.

The deduplication fingerprint is `types.Fingerprint(scope, data)`: the plain SHA-256 of the data in the default scope, so documents ingested before scopes existed still match. On pgstore, scope is the `rag_document.scope` column.

## Source Sync

`rag.SyncSource` reconciles a `types.Source` with the store by source URI. New URIs are ingested, changed content replaces the URI's document in place (its UUID survives), and unchanged content is skipped. With `Prune`, documents whose URI the source no longer returns are deleted.

```go
res, err := rag.SyncSource(ctx, pipe, &source.Filesystem{Dir: "docs", Recursive: true}, types.SyncOptions{
    Scope:       "acme",
    Since:       lastCursor,
    Prune:       true,
    PrunePrefix: "docs/",
})
lastCursor = res.Cursor
```

Content is compared by fingerprint whenever the source sends the bytes, as `source.Filesystem` and `source.HTTP` always do, so an edit that keeps an old modification time (`cp -p`, `rsync -a`, `tar x`) is still picked up. `Since` lets only a document without bytes (nil `Data`) count as unchanged by its time alone.

### Filesystem filters

`source.Filesystem` skips these by default:

| Rule | Default | Turn off with |
| --- | --- | --- |
| Tool directories | `.git`, `.hg`, `.svn`, `node_modules` (`source.DefaultSkipDirs`) | `IncludeToolDirs` |
| Dot files and dot directories | any name starting with `.` | `IncludeHidden` |
| Secret file names | `.env*`, `*.env`, `*.pem`, `*.key`, `id_rsa*`, `id_dsa*`, `id_ecdsa*`, `id_ed25519*`, `*.p12`, `*.pfx`, `*.kdbx`, `credentials*.json`, `*.tfstate*`, `.netrc`, `.npmrc`, `.pypirc` (`source.DefaultDenyPatterns`, case-insensitive) | `AllowSecretNames` |
| Ignore files | `.gitignore` and `.saigeignore` in every walked directory, with gitignore semantics: nested files, `!` negation, trailing `/` for directories, leading `/` anchors. `.saigeignore` wins over `.gitignore` in the same directory. | `NoIgnoreFiles` |

Then `Exclude` and `Include` (doublestar globs on the path relative to `Dir`, such as `docs/**/*.md`) and `Extensions` apply. A skipped directory is not walked, so nothing under it can be re-included. Ignore files above `Dir`, `.git/info/exclude`, and the global git excludes file are not read.

`Filesystem` implements `types.FilteringSource`, so `res.Skipped` lists every skipped file or directory with the rule that skipped it. A skipped URI is never pruned: changing a rule is not evidence that a file is gone, and a mistyped `Exclude` should not empty an index. To remove documents a new rule now skips, delete them by UUID.

The store must implement `types.SourceLister` (memstore and pgstore do); `types.SourceFinder` looks up one URI. A per-URI failure does not stop the sync: it is listed in `res.Failed` and joined into `err`.

Sources report when content last changed: `source.Filesystem` uses the file modification time and `source.HTTP` the `Last-Modified` header. The time is stored as `Document.SourceModifiedAt` and is the document's effective time (`Document.EffectiveTime`) for recency scoring and time-range filters, because re-ingesting an old file does not make it new.

## Chunking

| Strategy | Description |
|----------|-------------|
| Recursive | Tries separators (`\n\n`, `\n`, `. `, ` `) with configurable overlap |
| Semantic | Splits where embedding similarity drops below threshold |

```go
rag.WithRecursiveChunker(512, 50)       // maxSize, overlap
rag.WithSemanticChunker(0.1, 100, 1000) // threshold, minSize, maxSize
```

Every recursive chunk stays within `maxSize` tokens, overlap included. The overlap starts on a word boundary, and values above half of `maxSize` are clamped. Sections with several variants are emitted once: only long text variants are split, and images and short text stay on the first chunk.

## Embedding

Embedding calls carry a purpose on the context: `types.PurposeDocument` during ingest and `types.PurposeQuery` during vector search. An embedder for an asymmetric model reads it with `types.EmbedPurposeFrom(ctx)`. The embedding cache keys on content and purpose, not on variant or section UUIDs, so re-ingesting unchanged text reuses cached vectors.

Large documents can be split into bounded embedding requests:

```go
emb := embedderregistry.NewBatching(inner,
    embedderregistry.WithMaxBatchItems(256),
    embedderregistry.WithMaxBatchTokens(100_000),
    embedderregistry.WithMaxInputTokens(8192), // truncate oversized inputs
)
registry := embedderregistry.NewTextOnly(embeddingcache.New(emb))
```

Asymmetric models need different inputs for queries and documents. `embedderregistry.NewPurposeRouter(documentEmbedder, queryEmbedder)` routes by purpose, for example to two clients configured with Gemini `RETRIEVAL_DOCUMENT` and `RETRIEVAL_QUERY`. `embedderregistry.NewPrefixed(inner, "query: ", "passage: ")` adds the instruction prefixes that e5 and nomic-embed expect.

`embedderregistry.NewRetrying(inner, retry.Config{...})` retries rate limits, overloads, and network failures with exponential backoff and full jitter, and waits at least the provider's Retry-After. It uses the chat retry configuration (`agent/provider/retry.Config`) and error taxonomy (`agent/types.ProviderError`), never retries `ErrEmbeddingShape` or a cancelled context, and returns `*agent/types.RetryError` when attempts run out. Place it under the batching decorator so each batch is retried on its own.

## Retrieval

| Retriever | Description |
|-----------|-------------|
| Vector | Embed query, cosine similarity search |
| BM25 | On `pgstore`, a pg_search BM25 index in Postgres (`pgstore.NewKeywordRetriever`); on other stores, an in-memory inverted index with configurable K1/B |
| Graph | Knowledge graph facts resolved to document variants via episode provenance |
| Parent | Wraps any retriever, expands hits to full parent section context |

Multiple retrievers are combined via **Reciprocal Rank Fusion**. Hits that the parent retriever expands to the same section merge into one result, and context assembly skips passages whose text repeats an earlier one.

```go
rag.WithBM25(nil)          // default K1=1.2, B=0.75
rag.WithParentContext()    // expand to parent sections
```

Fusion is pluggable through `rag.WithFuser`. `fusion.RRF` is the default; `fusion.Weighted` trusts some retrievers more, keyed by retriever name (`vector`, `bm25`, `graph`, or any retriever implementing `types.Named`):

```go
rag.WithFuser(fusion.Weighted{Weights: map[string]float64{"vector": 2, "bm25": 1}})
```

Weights and the RRF constant can also be set for a whole pipeline, with any fuser, and overridden per search. A search's weights override the pipeline's key by key:

```go
pipe, _ := rag.NewPipeline(
    rag.WithStore(store), rag.WithEmbedders(embedders), rag.WithBM25(nil),
    rag.WithFusionWeights(map[string]float64{"vector": 1, "bm25": 2}),
    rag.WithFusionK(20),
)

// Trust lexical matches more for an identifier, and sharpen the ranks.
res, _ := pipe.Search(ctx, "ERR_CONN_RESET",
    types.WithFusionWeights(map[string]float64{"bm25": 4}),
    types.WithFusionK(5))
```

By default `fusion.Weighted` fuses ranks. With a `Normalization` it fuses scores instead: each retriever's raw scores are rescaled per list, then weighted and summed, so a hit far ahead in one list keeps its lead.

```go
rag.WithFuser(fusion.Weighted{Normalization: fusion.NormalizeMinMax}) // each list to [0,1]
rag.WithFuser(fusion.Weighted{Normalization: fusion.NormalizeZScore}) // each list to standard scores
```

### Keyword queries

A plain search hands the BM25 arm the query text: it is tokenized like the index and any term matches. `types.WithKeywordQuery` runs a structured query instead. Every field is plain text; nothing is parsed as query syntax, and pgstore binds each value as a parameter.

```go
// Exact phrase, allowing one extra word in between.
pipe.Search(ctx, "connection reset by peer",
    types.WithKeywordQuery(types.KeywordQuery{Mode: types.KeywordPhrase, Slop: 1}))

// Typo tolerance and prefixes: "okapy" matches "okapi", "postg" matches "postgres".
types.WithKeywordQuery(types.KeywordQuery{Fuzziness: 1, Transpositions: true})
types.WithKeywordQuery(types.KeywordQuery{Prefix: true, Mode: types.KeywordAll})

// Boolean clauses: the query text and Must are required, Should raises the
// score, MustNot excludes.
types.WithKeywordQuery(types.KeywordQuery{
    Must:    []types.KeywordClause{{Text: "postgres"}},
    Should:  []types.KeywordClause{{Text: "hnsw ef_search", Mode: types.KeywordPhrase}},
    MustNot: []types.KeywordClause{{Text: "mysql"}},
})

// Search the document title and section heading too, and weight them.
types.WithKeywordQuery(types.KeywordQuery{
    Fields: &types.FieldBoosts{Body: 1, Title: 3, Heading: 2},
})

// Return the matched words for citations.
types.WithKeywordQuery(types.KeywordQuery{
    Highlight: &types.HighlightOptions{StartTag: "**", EndTag: "**", MaxChars: 200},
})
```

An empty `Text` takes the search query; the query transformer's rewrites each get the same settings. `Fuzziness` is an edit distance from 0 to 2. `Slop` counts position moves: one extra word costs 1, two swapped words cost 2. A one-word phrase matches like a plain term. A clause matches in any searched field, and each field is matched on its own, so `KeywordAll` needs every term in one field. An invalid query fails the BM25 arm with `types.ErrInvalidKeywordQuery`.

With `Highlight`, pgstore sets `SearchHit.Highlight`: `Snippet` is the best fragment with matched words wrapped in the tags, and `Spans` are byte offsets of the matched words in `Variant.Text`. Parent and neighbor expansion move the spans into the widened text. pg_search does not highlight fuzzy or prefix matches, and a match only in the title or heading has no highlight.

| Feature | pgstore (pg_search) | in-memory BM25 |
|---------|--------------------|----------------|
| Any, all, phrase with slop | yes | yes |
| Prefix and fuzzy (with transpositions) | yes | yes |
| Must, Should, MustNot | yes | yes |
| Title and heading boosts | yes | body only |
| Highlight | yes | no |

Search options that shape the result:

| Option | Effect |
|--------|--------|
| `types.WithTimeRange(since, until)` | Keep hits whose document effective time is in `[since, until)`; a zero bound is open |
| `types.WithContentDedup()` | Collapse hits with identical trimmed text (or bytes), keeping the highest ranked |
| `types.WithNeighborWindow(n)` | Widen each text hit with up to `n` neighboring sections; `Provenance.Window` records the range, and a hit already inside a higher-ranked window is dropped |
| `types.WithScope(scope)` | Search one scope (see [Scopes](#scopes)) |
| `types.WithKeywordQuery(q)` | Run a structured query in the BM25 arm (see [Keyword queries](#keyword-queries)) |
| `types.WithFusionWeights(w)`, `types.WithFusionK(k)` | Per-search fusion weights and RRF constant |

`SearchPipelineResult.Retrievals` reports each retriever call: name, query index, hit count, duration, and error, so a slow, empty, or failing arm is visible without tracing.

On `pgstore`, `rag.WithBM25` searches the pg_search BM25 index in Postgres (`types.KeywordSearcher`): the index lives with the data, applies the same scope and filters as vector search, and needs no rebuild. A store decorator, such as a tracing wrapper, that does not implement `types.KeywordSearcher` itself should implement `Unwrap() types.Store` (`types.StoreUnwrapper`); `rag.WithBM25` follows it to the store's keyword search instead of falling back to an in-memory index. On other stores the BM25 index lives in process memory. Indexing a document again replaces its postings, and a replaced or deleted document leaves the index. Over a persistent store without keyword search, call `rag.RebuildIndex(ctx, pipe)` after startup; it needs a store that implements `types.DocumentLister` (memstore and pgstore do). BM25 hits carry the document timestamp and filter on merged document and variant metadata when the store implements `types.VariantRecordsGetter` or `types.VariantRecordGetter` (memstore and pgstore implement both). BM25 resolves ranked candidates in batches, one store lookup per batch, and returns store errors other than a missing variant. Index and Remove calls made during `RebuildIndex` are replayed onto the new index before it is swapped in.

The graph retriever resolves each fact to the variant named by its asserting episode's `variant_uuid` metadata, then applies `ContentTypes` and metadata filters to that variant. A fact whose stored variant exists but fails the search's scope, time range, or filters is dropped. A fact that resolves to no variant becomes a text hit built from the fact, unless filters or a time range are set, `ContentTypes` excludes text, or the search names a scope other than the retriever's graph group; then it is dropped. A partial graph failure returns the surviving hits with `types.ErrPartialSearch`.

`rag.WithGraphNamespace(ns)` sets the graph group that documents are ingested into and that graph search is limited to. The host supplies it as the tenant scope. Each episode also records its document UUID, so one entity named in several documents is one node, and deleting a document removes only the relations no other document asserts. A graph without document deletion and without a namespace keeps one group per document instead.

`types.WithMinScore` takes a value in [0,1]: the fused score divided by the score of a hit ranked first by every retriever. Thresholds on raw cosine or BM25 scores belong in the retriever.

A query transformer failure does not fail the search. The pipeline searches with the queries it has (at least the original) and returns `types.ErrPartialSearch` with the result.

## Reranking

| Reranker | Description |
|----------|-------------|
| MMR | Maximal Marginal Relevance: balances relevance and diversity |
| Cross-Encoder | Pair-wise scoring via custom `Scorer` interface |

```go
rag.WithMMR(0.7)               // lambda=0.7
rag.WithCrossEncoder(myScorer) // custom scorer
```

With a reranker, each retriever returns a **candidate pool** of `max(4*limit, 20)` hits, the reranker orders the fused pool, and the pipeline returns the top `limit`. Set the pool per search with `types.WithCandidatePool(k)`. MMR scales relevance to [0,1] across its input, treats hits without embeddings as redundant, and keeps each hit's original score.

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

## Observability

`rag.WithObserver` sends spans and metrics to a `types.Observer`. The rag packages have no telemetry dependency; [`rag/otel`](otel/) adapts OpenTelemetry:

```go
obs, err := ragotel.NewObserver(ragotel.Config{TracerProvider: tp, MeterProvider: mp})
pipe, _ := rag.NewPipeline(..., rag.WithObserver(obs))
graph, _ := knowledge.NewGraph(ctx, ..., knowledge.WithObserver(obs))
```

A search opens `rag.search` with children `rag.transform`, one `rag.retrieve` per retriever and query (with `rag.retriever` and `rag.hits`), `rag.fuse`, `rag.rerank`, `rag.expand`, and `rag.assemble`. An ingest opens `rag.ingest` with `rag.extract`, `rag.chunk`, `rag.embed`, `rag.write`, `rag.index`, and `rag.graph`. Query text is never recorded. Metrics cover embedding calls (`rag.embedding.duration`, `rag.embedding.inputs`) and retriever calls (`rag.retrieval.duration`, `rag.retrieval.hits`). Wrap a provider embedder in `embedderregistry.NewObserved` to see it under its own name.

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

// Label by source URI so the golden set survives re-ingest. Variant UUIDs
// (RelevantUUIDs) change every time a document is ingested again.
cases := []eval.EvalCase{{
    Query:        "attention mechanism",
    RelevantKeys: []string{"https://example.com/paper.pdf"},
    RelevanceKey: eval.RelevanceSource,
}}

// Full evaluation pipeline with functional options. Every retrieval metric
// uses the top K hits. A case without labels sets Unlabeled instead of
// scoring zero, and a partial search failure is kept in RetrievalWarning.
// An unknown RelevanceKey fails with eval.ErrUnknownRelevanceKey.
results, _ := eval.Evaluate(ctx, cases, pipeline,
    eval.WithLLM(llm),
    eval.WithEmbedders(embedders),
    eval.WithK(10),
    eval.WithJudgeRubric("Score helpfulness, accuracy, and completeness."),
)
```

`TestHybridRetrievalEval` (`rag/eval/hybrid_eval_test.go`) compares BM25 alone, vector search alone, and hybrid fusion (RRF, min-max, z-score) on a fixed corpus of 34 passages and 23 labeled queries, with recorded embeddings so it runs offline. Run it with `go test -run TestHybridRetrievalEval -v ./rag/eval/` to print the table; `SAIGE_EVAL_REGENERATE=1` with `OPENAI_API_KEY` records the embeddings again.

## Agent Tool Bindings

The [`rag/tool`](tool/) package exposes the pipeline as agent tools:

```go
import ragtool "github.com/urmzd/saige/rag/tool"

ragTools := ragtool.NewTools(pipeline)
// rag_search, rag_lookup, rag_update, rag_delete, rag_reconstruct
```

`rag_search` returns provenance only, at most 50 hits, and reports a partial retriever failure as `{hits, degraded, warnings}`. `rag_lookup` and `rag_reconstruct` never return embeddings or raw bytes, and cap text at 16 KiB per variant and the encoded output at 64 KiB per call (`ragtool.WithOutputLimits`). The per-call cap covers every entry, including variants with no text and all metadata; dropped metadata entries are marked `metadata_truncated`. `rag_lookup` with `section: true` returns the parent section text that search used.

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
- [`rag/knowledge`](knowledge/README.md): knowledge graph SDK backing the graph retriever
- [Root README](../README.md): project overview and installation
