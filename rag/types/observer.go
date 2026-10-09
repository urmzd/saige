package types

import (
	"context"
	"time"
)

// Span names the pipeline opens through an Observer. A search opens
// SpanSearch with one child per stage and one SpanRetrieve per retriever and
// query; an ingest opens SpanIngest with one child per stage.
const (
	SpanSearch    = "rag.search"
	SpanTransform = "rag.transform"
	SpanRetrieve  = "rag.retrieve"
	SpanFuse      = "rag.fuse"
	SpanRerank    = "rag.rerank"
	SpanExpand    = "rag.expand"
	SpanAssemble  = "rag.assemble"

	SpanIngest  = "rag.ingest"
	SpanExtract = "rag.extract"
	SpanChunk   = "rag.chunk"
	SpanEmbed   = "rag.embed"
	SpanWrite   = "rag.write"
	SpanIndex   = "rag.index"
	SpanGraph   = "rag.graph"

	SpanSync = "rag.sync"
)

// Attribute keys the pipeline sets on spans. Query text is never recorded,
// because it can carry user data.
const (
	AttrScope        = "rag.scope"
	AttrRetriever    = "rag.retriever"
	AttrEmbedder     = "rag.embedder"
	AttrQueryIndex   = "rag.query_index"
	AttrQueries      = "rag.queries"
	AttrHits         = "rag.hits"
	AttrCandidates   = "rag.candidates"
	AttrDocumentUUID = "rag.document_uuid"
	AttrSections     = "rag.sections"
	AttrVariants     = "rag.variants"
	AttrInputs       = "rag.inputs"
	AttrDeduplicated = "rag.deduplicated"
	AttrPartial      = "rag.partial"
)

// Attribute is a span attribute. Value is a string, bool, int, int64, or
// float64; adapters may drop other types.
type Attribute struct {
	Key   string
	Value any
}

// Attr builds an Attribute.
func Attr(key string, value any) Attribute { return Attribute{Key: key, Value: value} }

// Span is one timed operation started by an Observer.
type Span interface {
	SetAttributes(attrs ...Attribute)
	// RecordError marks the span failed with err. A nil err is ignored.
	RecordError(err error)
	End()
}

// EmbeddingRecord describes one embedding call.
type EmbeddingRecord struct {
	// Embedder is the embedder's name (see Named), or a stage name such as
	// "ingest" when the pipeline records a whole registry call.
	Embedder string
	Purpose  EmbedPurpose
	Inputs   int
	Duration time.Duration
	Err      error
}

// RetrievalRecord describes one retriever call.
type RetrievalRecord struct {
	Retriever string
	Hits      int
	Duration  time.Duration
	Err       error
}

// Observer receives traces and metrics from the RAG pipeline and its
// decorators. It keeps rag free of any telemetry dependency: the rag/otel
// package adapts it to OpenTelemetry, and NoopObserver discards everything.
// Implementations must be safe for concurrent use, because retrievers run in
// parallel.
type Observer interface {
	// StartSpan starts a span as a child of any span in ctx and returns a
	// context that carries the new span.
	StartSpan(ctx context.Context, name string, attrs ...Attribute) (context.Context, Span)
	RecordEmbedding(ctx context.Context, rec EmbeddingRecord)
	RecordRetrieval(ctx context.Context, rec RetrievalRecord)
}

// NoopObserver is an Observer that records nothing. Embed it to implement
// only part of Observer.
type NoopObserver struct{}

var _ Observer = NoopObserver{}

// StartSpan returns ctx and a span that does nothing.
func (NoopObserver) StartSpan(ctx context.Context, _ string, _ ...Attribute) (context.Context, Span) {
	return ctx, noopSpan{}
}

// RecordEmbedding does nothing.
func (NoopObserver) RecordEmbedding(context.Context, EmbeddingRecord) {}

// RecordRetrieval does nothing.
func (NoopObserver) RecordRetrieval(context.Context, RetrievalRecord) {}

type noopSpan struct{}

func (noopSpan) SetAttributes(...Attribute) {}
func (noopSpan) RecordError(error)          {}
func (noopSpan) End()                       {}
