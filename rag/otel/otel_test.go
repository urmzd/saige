package otel_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/urmzd/saige/rag"
	"github.com/urmzd/saige/rag/bm25retriever"
	"github.com/urmzd/saige/rag/memstore"
	ragotel "github.com/urmzd/saige/rag/otel"
	ragtypes "github.com/urmzd/saige/rag/types"
	"github.com/urmzd/saige/rag/vectorretriever"
)

type spyProvider struct {
	noop.TracerProvider
	tracer *spyTracer
}

func (p spyProvider) Tracer(string, ...trace.TracerOption) trace.Tracer { return p.tracer }

type spyTracer struct {
	noop.Tracer
	mu    sync.Mutex
	spans []*spySpan
}

func (t *spyTracer) Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	cfg := trace.NewSpanStartConfig(opts...)
	s := &spySpan{name: name, attrs: map[attribute.Key]attribute.Value{}}
	if parent, ok := trace.SpanFromContext(ctx).(*spySpan); ok {
		s.parent = parent.name
	}
	for _, kv := range cfg.Attributes() {
		s.attrs[kv.Key] = kv.Value
	}
	t.mu.Lock()
	t.spans = append(t.spans, s)
	t.mu.Unlock()
	return trace.ContextWithSpan(ctx, s), s
}

type spySpan struct {
	noop.Span
	mu     sync.Mutex
	name   string
	parent string
	attrs  map[attribute.Key]attribute.Value
	status codes.Code
	desc   string
	ended  bool
}

func (s *spySpan) SetAttributes(kv ...attribute.KeyValue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range kv {
		s.attrs[a.Key] = a.Value
	}
}
func (s *spySpan) SetStatus(c codes.Code, d string) { s.status, s.desc = c, d }
func (s *spySpan) End(...trace.SpanEndOption)       { s.ended = true }

func newObserver(t *testing.T, redactor func(string) string) (*ragotel.Observer, *spyTracer) {
	t.Helper()
	tracer := &spyTracer{}
	obs, err := ragotel.NewObserver(ragotel.Config{
		TracerProvider: spyProvider{tracer: tracer},
		MeterProvider:  metricnoop.NewMeterProvider(),
		Redactor:       redactor,
	})
	if err != nil {
		t.Fatal(err)
	}
	return obs, tracer
}

func TestObserverSpans(t *testing.T) {
	obs, tracer := newObserver(t, func(s string) string { return strings.ReplaceAll(s, "secret", "***") })
	ctx, parent := obs.StartSpan(context.Background(), ragtypes.SpanSearch,
		ragtypes.Attr("s", "v"), ragtypes.Attr("b", true), ragtypes.Attr("i", 3),
		ragtypes.Attr("i64", int64(4)), ragtypes.Attr("f", 1.5), ragtypes.Attr("d", time.Second))
	_, child := obs.StartSpan(ctx, ragtypes.SpanRetrieve)
	child.SetAttributes(ragtypes.Attr(ragtypes.AttrHits, 2))
	child.RecordError(errors.New("secret leaked"))
	child.RecordError(nil)
	child.End()
	parent.End()

	if len(tracer.spans) != 2 {
		t.Fatalf("spans = %d", len(tracer.spans))
	}
	p, c := tracer.spans[0], tracer.spans[1]
	tests := []struct {
		name string
		got  attribute.Value
		want attribute.Value
	}{
		{"string", p.attrs["s"], attribute.StringValue("v")},
		{"bool", p.attrs["b"], attribute.BoolValue(true)},
		{"int", p.attrs["i"], attribute.IntValue(3)},
		{"int64", p.attrs["i64"], attribute.Int64Value(4)},
		{"float64", p.attrs["f"], attribute.Float64Value(1.5)},
		{"other types use fmt", p.attrs["d"], attribute.StringValue("1s")},
		{"set later", c.attrs[ragtypes.AttrHits], attribute.IntValue(2)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got %v, want %v", tt.got.String(), tt.want.String())
			}
		})
	}
	if c.parent != ragtypes.SpanSearch || !c.ended || !p.ended {
		t.Errorf("child parent=%q ended=%v parent ended=%v", c.parent, c.ended, p.ended)
	}
	if c.status != codes.Error || c.desc != "*** leaked" {
		t.Errorf("status = %v %q, want redacted error", c.status, c.desc)
	}

	obs.RecordEmbedding(context.Background(), ragtypes.EmbeddingRecord{Embedder: "e", Inputs: 2, Duration: time.Millisecond})
	obs.RecordRetrieval(context.Background(), ragtypes.RetrievalRecord{Retriever: "r", Hits: 1, Err: errors.New("x")})
}

// tableEmbedder embeds text by its first letter.
type tableEmbedder struct{}

func (tableEmbedder) Register(ragtypes.ContentType, ragtypes.VariantEmbedder) {}
func (tableEmbedder) Embed(_ context.Context, variants []ragtypes.ContentVariant) ([][]float32, error) {
	out := make([][]float32, len(variants))
	for i, v := range variants {
		out[i] = []float32{float32(len(v.Text)), 1}
	}
	return out, nil
}

type identityReranker struct{}

func (identityReranker) Rerank(_ context.Context, _ string, hits []ragtypes.SearchHit) ([]ragtypes.SearchHit, error) {
	return hits, nil
}

type oneSection struct{}

func (oneSection) Extract(_ context.Context, raw *ragtypes.RawDocument) (*ragtypes.Document, error) {
	return &ragtypes.Document{UUID: "d1", SourceURI: raw.SourceURI, Sections: []ragtypes.Section{{
		UUID: "s1", DocumentUUID: "d1",
		Variants: []ragtypes.ContentVariant{{UUID: "v1", SectionUUID: "s1", ContentType: ragtypes.ContentText, Text: string(raw.Data)}},
	}}}, nil
}

// TestPipelineSearchSpans checks the span tree a hybrid search with a
// reranker produces through the OpenTelemetry adapter.
func TestPipelineSearchSpans(t *testing.T) {
	ctx := context.Background()
	obs, tracer := newObserver(t, nil)
	store := memstore.New()
	emb := tableEmbedder{}
	pipe, err := rag.New(rag.Config{},
		rag.WithStore(store),
		rag.WithContentExtractor(oneSection{}),
		rag.WithEmbedders(emb),
		rag.WithRetrievers(vectorretriever.New(store, emb), bm25retriever.New(store, nil)),
		rag.WithReranker(identityReranker{}),
		rag.WithObserver(obs),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pipe.Ingest(ctx, &ragtypes.RawDocument{SourceURI: "u", Data: []byte("traced words")}); err != nil {
		t.Fatal(err)
	}
	tracer.spans = nil
	if _, err := pipe.Search(ctx, "traced"); err != nil {
		t.Fatal(err)
	}
	children := map[string][]*spySpan{}
	for _, s := range tracer.spans {
		if s.parent == ragtypes.SpanSearch {
			children[s.name] = append(children[s.name], s)
		}
	}
	if len(children[ragtypes.SpanRetrieve]) != 2 || len(children[ragtypes.SpanRerank]) != 1 {
		t.Fatalf("search children = %v", children)
	}
	for _, s := range children[ragtypes.SpanRetrieve] {
		if s.attrs[ragtypes.AttrHits].AsInt64() != 1 {
			t.Errorf("retrieve span %v hits = %v", s.attrs[ragtypes.AttrRetriever].String(), s.attrs[ragtypes.AttrHits].String())
		}
	}
}
