// Package otel adapts OpenTelemetry to the rag Observer interface, so a RAG
// pipeline (rag.WithObserver), a knowledge graph (knowledge.WithObserver),
// and observed embedders (embedderregistry.NewObserved) emit spans and
// metrics without rag itself depending on OpenTelemetry.
//
// Spans follow the names in rag/types (rag.search, rag.retrieve, rag.ingest,
// and their stages). Metrics:
//
//   - rag.embedding.duration (s), histogram, by rag.embedder, rag.purpose, error
//   - rag.embedding.inputs, counter, by rag.embedder, rag.purpose
//   - rag.retrieval.duration (s), histogram, by rag.retriever, error
//   - rag.retrieval.hits, histogram, by rag.retriever
package otel

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	ragtypes "github.com/urmzd/saige/rag/types"
)

// Config configures the adapter.
type Config struct {
	// TracerProvider to use. If nil, the global provider is used.
	TracerProvider trace.TracerProvider
	// MeterProvider to use. If nil, the global provider is used.
	MeterProvider metric.MeterProvider
	// ServiceName names the tracer and meter. Defaults to "saige".
	ServiceName string
	// Redactor, when set, rewrites every error message before it is written
	// to a span. Store and provider errors can embed document text.
	Redactor func(string) string
}

// Observer implements ragtypes.Observer with OpenTelemetry.
type Observer struct {
	tracer   trace.Tracer
	redactor func(string) string

	embedDuration metric.Float64Histogram
	embedInputs   metric.Int64Counter
	retrDuration  metric.Float64Histogram
	retrHits      metric.Int64Histogram
}

var _ ragtypes.Observer = (*Observer)(nil)

// NewObserver builds an Observer. It fails only when the meter cannot
// create its instruments.
func NewObserver(cfg Config) (*Observer, error) {
	name := cfg.ServiceName
	if name == "" {
		name = "saige"
	}
	tp := cfg.TracerProvider
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	mp := cfg.MeterProvider
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	meter := mp.Meter(name)

	o := &Observer{tracer: tp.Tracer(name), redactor: cfg.Redactor}
	var err error
	if o.embedDuration, err = meter.Float64Histogram("rag.embedding.duration",
		metric.WithUnit("s"), metric.WithDescription("Duration of embedding calls")); err != nil {
		return nil, fmt.Errorf("rag/otel: %w", err)
	}
	if o.embedInputs, err = meter.Int64Counter("rag.embedding.inputs",
		metric.WithDescription("Inputs sent to embedders")); err != nil {
		return nil, fmt.Errorf("rag/otel: %w", err)
	}
	if o.retrDuration, err = meter.Float64Histogram("rag.retrieval.duration",
		metric.WithUnit("s"), metric.WithDescription("Duration of retriever calls")); err != nil {
		return nil, fmt.Errorf("rag/otel: %w", err)
	}
	if o.retrHits, err = meter.Int64Histogram("rag.retrieval.hits",
		metric.WithDescription("Hits returned by retriever calls")); err != nil {
		return nil, fmt.Errorf("rag/otel: %w", err)
	}
	return o, nil
}

// StartSpan implements ragtypes.Observer.
func (o *Observer) StartSpan(ctx context.Context, name string, attrs ...ragtypes.Attribute) (context.Context, ragtypes.Span) {
	ctx, span := o.tracer.Start(ctx, name, trace.WithAttributes(convert(attrs)...))
	return ctx, &otelSpan{span: span, redactor: o.redactor}
}

// RecordEmbedding implements ragtypes.Observer.
func (o *Observer) RecordEmbedding(ctx context.Context, rec ragtypes.EmbeddingRecord) {
	base := []attribute.KeyValue{
		attribute.String("rag.embedder", rec.Embedder),
		attribute.String("rag.purpose", string(rec.Purpose)),
	}
	o.embedInputs.Add(ctx, int64(rec.Inputs), metric.WithAttributes(base...))
	o.embedDuration.Record(ctx, rec.Duration.Seconds(),
		metric.WithAttributes(append(base, attribute.Bool("error", rec.Err != nil))...))
}

// RecordRetrieval implements ragtypes.Observer.
func (o *Observer) RecordRetrieval(ctx context.Context, rec ragtypes.RetrievalRecord) {
	name := attribute.String("rag.retriever", rec.Retriever)
	o.retrHits.Record(ctx, int64(rec.Hits), metric.WithAttributes(name))
	o.retrDuration.Record(ctx, rec.Duration.Seconds(),
		metric.WithAttributes(name, attribute.Bool("error", rec.Err != nil)))
}

type otelSpan struct {
	span     trace.Span
	redactor func(string) string
}

func (s *otelSpan) SetAttributes(attrs ...ragtypes.Attribute) {
	s.span.SetAttributes(convert(attrs)...)
}

func (s *otelSpan) RecordError(err error) {
	if err == nil {
		return
	}
	msg := err.Error()
	if s.redactor != nil {
		msg = s.redactor(msg)
	}
	s.span.AddEvent("exception", trace.WithAttributes(
		attribute.String("exception.type", fmt.Sprintf("%T", err)),
		attribute.String("exception.message", msg),
	))
	s.span.SetStatus(codes.Error, msg)
}

func (s *otelSpan) End() { s.span.End() }

// convert maps rag attributes to OpenTelemetry ones. Values of other types
// are recorded with their fmt representation.
func convert(attrs []ragtypes.Attribute) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(attrs))
	for _, a := range attrs {
		switch v := a.Value.(type) {
		case string:
			out = append(out, attribute.String(a.Key, v))
		case bool:
			out = append(out, attribute.Bool(a.Key, v))
		case int:
			out = append(out, attribute.Int(a.Key, v))
		case int64:
			out = append(out, attribute.Int64(a.Key, v))
		case float64:
			out = append(out, attribute.Float64(a.Key, v))
		default:
			out = append(out, attribute.String(a.Key, fmt.Sprint(v)))
		}
	}
	return out
}
