package embedderregistry_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/urmzd/saige/rag/embedderregistry"
	"github.com/urmzd/saige/rag/types"
)

type stringEmbedder struct {
	got []string
	err error
}

func (s *stringEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	s.got = texts
	if s.err != nil {
		return nil, s.err
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = []float32{float32(len(t))}
	}
	return out, nil
}

func TestTextEmbedsVariantTextInOrder(t *testing.T) {
	inner := &stringEmbedder{}
	vecs, err := embedderregistry.Text(inner).Embed(context.Background(), []types.ContentVariant{
		{ContentType: types.ContentText, Text: "a"},
		{ContentType: types.ContentTable, Text: "bbb"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(inner.got, []string{"a", "bbb"}) || !reflect.DeepEqual(vecs, [][]float32{{1}, {3}}) {
		t.Fatalf("texts %v vectors %v", inner.got, vecs)
	}
	// Behind a text-only registry it serves every content type.
	vecs, err = embedderregistry.NewTextOnly(embedderregistry.Text(inner)).Embed(context.Background(),
		[]types.ContentVariant{{ContentType: types.ContentTable, Text: "cc"}})
	if err != nil || !reflect.DeepEqual(vecs, [][]float32{{2}}) {
		t.Fatalf("registry: %v %v", vecs, err)
	}
}

func TestTextReturnsInnerError(t *testing.T) {
	want := errors.New("rate limited")
	if _, err := embedderregistry.Text(&stringEmbedder{err: want}).Embed(context.Background(),
		[]types.ContentVariant{{Text: "a"}}); !errors.Is(err, want) {
		t.Fatalf("got %v", err)
	}
}
