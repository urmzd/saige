package chunker

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urmzd/saige/rag/types"
)

func makeTestSection(size int) types.Section {
	seed := "The quick brown fox jumps over the lazy dog. "
	words := strings.Repeat(seed, size/len(seed)+1)
	return types.Section{
		UUID: "bench-section",
		Variants: []types.ContentVariant{{
			UUID:        "bench-variant",
			ContentType: types.ContentText,
			Text:        words[:size],
		}},
	}
}

func BenchmarkRecursiveChunker(b *testing.B) {
	for _, size := range []int{500, 5000, 50000} {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			sec := makeTestSection(size)
			doc := &types.Document{
				UUID:     "bench-doc",
				Sections: []types.Section{sec},
			}
			c := NewRecursive(&Config{MaxTokens: 512, Overlap: 50})

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				c.Chunk(context.Background(), doc)
			}
		})
	}
}
