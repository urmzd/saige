package embedderregistry_test

import (
	"context"
	"errors"
	"testing"

	"github.com/urmzd/saige/rag/embedderregistry"
	"github.com/urmzd/saige/rag/types"
)

// textRecorder records the texts it is asked to embed.
type textRecorder struct {
	texts []string
	err   error
}

func (r *textRecorder) Embed(_ context.Context, variants []types.ContentVariant) ([][]float32, error) {
	out := make([][]float32, len(variants))
	for i, v := range variants {
		r.texts = append(r.texts, v.Text)
		out[i] = []float32{1}
	}
	return out, r.err
}

func TestPurposeRouterAndPrefixed(t *testing.T) {
	tests := []struct {
		name    string
		purpose types.EmbedPurpose
		wantDoc []string
		wantQry []string
		wantPfx string
	}{
		{"query", types.PurposeQuery, nil, []string{"hi"}, "query: hi"},
		{"document", types.PurposeDocument, []string{"hi"}, nil, "passage: hi"},
		{"unspecified uses document", types.PurposeUnspecified, []string{"hi"}, nil, "passage: hi"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := types.WithEmbedPurpose(context.Background(), tt.purpose)
			doc, qry := &textRecorder{}, &textRecorder{}
			variants := []types.ContentVariant{{ContentType: types.ContentText, Text: "hi"}}
			if _, err := embedderregistry.NewPurposeRouter(doc, qry).Embed(ctx, variants); err != nil {
				t.Fatal(err)
			}
			if len(doc.texts) != len(tt.wantDoc) || len(qry.texts) != len(tt.wantQry) {
				t.Errorf("document=%v query=%v", doc.texts, qry.texts)
			}

			inner := &textRecorder{}
			p := embedderregistry.NewPrefixed(inner, "query: ", "passage: ")
			img := types.ContentVariant{ContentType: types.ContentImage, Text: "caption"}
			if _, err := p.Embed(ctx, append(variants, img)); err != nil {
				t.Fatal(err)
			}
			if inner.texts[0] != tt.wantPfx || inner.texts[1] != "caption" {
				t.Errorf("prefixed texts = %q", inner.texts)
			}
			if variants[0].Text != "hi" {
				t.Error("caller's variant was modified")
			}
		})
	}
	if _, err := embedderregistry.NewPurposeRouter(&textRecorder{}, nil).Embed(
		types.WithEmbedPurpose(context.Background(), types.PurposeQuery), []types.ContentVariant{{Text: "x"}}); err != nil {
		t.Errorf("nil query embedder must fall back to the document embedder: %v", err)
	}
}

type countingObserver struct {
	types.NoopObserver
	records []types.EmbeddingRecord
	spans   []string
}

func (o *countingObserver) StartSpan(ctx context.Context, name string, attrs ...types.Attribute) (context.Context, types.Span) {
	o.spans = append(o.spans, name)
	return o.NoopObserver.StartSpan(ctx, name, attrs...)
}

func (o *countingObserver) RecordEmbedding(_ context.Context, rec types.EmbeddingRecord) {
	o.records = append(o.records, rec)
}

func TestObserved(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name    string
		err     error
		wantErr bool
	}{
		{"success", nil, false},
		{"failure", boom, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obs := &countingObserver{}
			e := embedderregistry.NewObserved(&textRecorder{err: tt.err}, "gemini", obs)
			ctx := types.WithEmbedPurpose(context.Background(), types.PurposeQuery)
			_, err := e.Embed(ctx, []types.ContentVariant{{Text: "a"}, {Text: "b"}})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v", err)
			}
			if e.Name() != "gemini" || len(obs.spans) != 1 || obs.spans[0] != types.SpanEmbed {
				t.Errorf("name=%s spans=%v", e.Name(), obs.spans)
			}
			rec := obs.records[0]
			if rec.Embedder != "gemini" || rec.Inputs != 2 || rec.Purpose != types.PurposeQuery || (rec.Err != nil) != tt.wantErr {
				t.Errorf("record = %+v", rec)
			}
		})
	}
}
