package bm25retriever

import (
	"context"
	"fmt"
	"testing"

	"github.com/urmzd/graph-agent-dev-kit/rag/ragtypes"
)

func BenchmarkBM25Index(b *testing.B) {
	ctx := context.Background()
	r := New(nil, nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Index(ctx, &ragtypes.Document{
			UUID: fmt.Sprintf("doc-%d", i),
			Sections: []ragtypes.Section{{
				UUID: fmt.Sprintf("sec-%d", i),
				Variants: []ragtypes.ContentVariant{{
					UUID: fmt.Sprintf("var-%d", i),
					Text: "The quick brown fox jumps over the lazy dog in a benchmark test",
				}},
			}},
		})
	}
}

func BenchmarkBM25Retrieve(b *testing.B) {
	ctx := context.Background()
	r := New(nil, nil)

	for i := range 100 {
		r.Index(ctx, &ragtypes.Document{
			UUID: fmt.Sprintf("doc-%d", i),
			Sections: []ragtypes.Section{{
				UUID: fmt.Sprintf("sec-%d", i),
				Variants: []ragtypes.ContentVariant{{
					UUID: fmt.Sprintf("var-%d", i),
					Text: fmt.Sprintf("Document %d discusses machine learning and artificial intelligence", i),
				}},
			}},
		})
	}

	opts := &ragtypes.SearchOptions{Limit: 10}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Retrieve(ctx, "machine learning", opts)
	}
}
