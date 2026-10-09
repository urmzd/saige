package main

import (
	"encoding/json"
	"strings"
	"testing"

	ragtypes "github.com/urmzd/saige/rag/types"
)

func TestRagSearchOutputOmitsEmbeddings(t *testing.T) {
	result := &ragtypes.SearchPipelineResult{Query: "q", Hits: []ragtypes.SearchHit{{
		Variant: ragtypes.ContentVariant{UUID: "v1", Text: "hello", Embedding: []float32{0.25, 0.5}},
		Score:   0.9,
	}}}
	raw, err := json.Marshal(withoutEmbeddings(result))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "embedding") {
		t.Fatalf("output carries an embedding: %s", raw)
	}
	if !strings.Contains(string(raw), "hello") {
		t.Fatalf("output lost the hit text: %s", raw)
	}
	if len(result.Hits[0].Variant.Embedding) != 2 {
		t.Fatal("the caller's result was modified")
	}
	if withoutEmbeddings(nil) != nil {
		t.Fatal("nil result")
	}
}
