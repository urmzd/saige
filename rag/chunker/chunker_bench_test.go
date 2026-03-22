package chunker

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urmzd/graph-agent-dev-kit/rag/ragtypes"
)

func makeTestSection(size int) ragtypes.Section {
	words := strings.Repeat("The quick brown fox jumps over the lazy dog. ", size/46+1)
	return ragtypes.Section{
		UUID: "bench-section",
		Variants: []ragtypes.ContentVariant{{
			UUID:        "bench-variant",
			ContentType: ragtypes.ContentText,
			Text:        words[:size],
		}},
	}
}

func BenchmarkRecursiveChunker(b *testing.B) {
	for _, size := range []int{500, 5000, 50000} {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			sec := makeTestSection(size)
			doc := &ragtypes.Document{
				UUID:     "bench-doc",
				Sections: []ragtypes.Section{sec},
			}
			c := NewRecursive(&Config{MaxTokens: 512, Overlap: 50})

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				c.Chunk(context.Background(), doc)
			}
		})
	}
}
