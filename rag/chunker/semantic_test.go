package chunker_test

import (
	"context"
	"math"
	"testing"

	"github.com/urmzd/saige/rag/chunker"
	"github.com/urmzd/saige/rag/types"
)

// mockEmbedder returns predictable embeddings: alternating similar/dissimilar.
type mockEmbedder struct{}

func (m *mockEmbedder) Register(_ types.ContentType, _ types.VariantEmbedder) {}

func (m *mockEmbedder) Embed(_ context.Context, variants []types.ContentVariant) ([][]float32, error) {
	embeddings := make([][]float32, len(variants))
	for i := range variants {
		vec := make([]float32, 4)
		// Even indices get [1,0,0,0], odd get [0,1,0,0]: creating low similarity boundaries.
		if i%2 == 0 {
			vec[0] = 1.0
		} else {
			vec[1] = 1.0
		}
		embeddings[i] = vec
	}
	return embeddings, nil
}

func TestSemanticChunkerSplits(t *testing.T) {
	// Text with multiple sentences. Mock embedder will cause splits between consecutive sentences.
	text := "First sentence about cats. Second sentence about dogs. Third sentence about birds. Fourth sentence about fish."

	doc := &types.Document{
		UUID: "doc1",
		Sections: []types.Section{{
			UUID:         "sec1",
			DocumentUUID: "doc1",
			Variants: []types.ContentVariant{{
				UUID:        "var1",
				SectionUUID: "sec1",
				ContentType: types.ContentText,
				Text:        text,
			}},
		}},
	}

	cfg := &chunker.SemanticConfig{
		Threshold: 0.5, // Above zero similarity between orthogonal vectors.
		MinTokens: 1,
		MaxTokens: 512,
	}
	c := chunker.NewSemantic(&mockEmbedder{}, cfg)
	result, err := c.Chunk(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}

	// With orthogonal alternating embeddings, similarity is 0 between consecutive sentences,
	// which is below threshold 0.5, so each sentence should be its own chunk.
	// But MinTokens=1 means merging won't happen.
	if len(result.Sections) < 2 {
		t.Fatalf("expected multiple sections from semantic split, got %d", len(result.Sections))
	}
}

func TestSemanticChunkerShortText(t *testing.T) {
	// Short text below MinTokens should not be split.
	text := "Short."
	doc := &types.Document{
		UUID: "doc1",
		Sections: []types.Section{{
			UUID:         "sec1",
			DocumentUUID: "doc1",
			Variants: []types.ContentVariant{{
				UUID:        "var1",
				SectionUUID: "sec1",
				ContentType: types.ContentText,
				Text:        text,
			}},
		}},
	}

	cfg := &chunker.SemanticConfig{MinTokens: 50, MaxTokens: 512}
	c := chunker.NewSemantic(&mockEmbedder{}, cfg)
	result, err := c.Chunk(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Sections) != 1 {
		t.Fatalf("expected 1 section for short text, got %d", len(result.Sections))
	}
}

// Silence unused import warning for math.
var _ = math.Sqrt
