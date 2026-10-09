package chunker

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/urmzd/saige/rag/types"
)

// randomText builds text from short lowercase words joined by spaces,
// newlines, and paragraph breaks, so every word fits well inside any budget
// used below and a hard split never runs.
func randomText(rng *rand.Rand, words int) string {
	var b strings.Builder
	for i := 0; i < words; i++ {
		if i > 0 {
			switch rng.Intn(20) {
			case 0:
				b.WriteString("\n\n")
			case 1:
				b.WriteString("\n")
			default:
				b.WriteString(" ")
			}
		}
		fmt.Fprintf(&b, "w%d", i)
	}
	return b.String()
}

func TestRecursiveChunkerOverlapRespectsBudgetAndWords(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	configs := []struct{ maxTokens, overlap int }{
		{100, 20}, {50, 10}, {30, 29}, {64, 0}, {40, 40}, {20, 5},
	}
	for _, cfg := range configs {
		for trial := 0; trial < 5; trial++ {
			name := fmt.Sprintf("max=%d/overlap=%d/trial=%d", cfg.maxTokens, cfg.overlap, trial)
			t.Run(name, func(t *testing.T) {
				text := randomText(rng, 100+rng.Intn(400))
				sourceWords := make(map[string]bool)
				for _, w := range strings.Fields(text) {
					sourceWords[w] = true
				}

				c := NewRecursive(&Config{
					MaxTokens:  cfg.maxTokens,
					Overlap:    cfg.overlap,
					Separators: []string{"\n\n", "\n", ". ", " "},
				})
				result, err := c.Chunk(context.Background(), makeEdgeDoc(text))
				if err != nil {
					t.Fatal(err)
				}
				if len(result.Sections) < 2 {
					t.Fatalf("expected the text to split, got %d section(s)", len(result.Sections))
				}
				for i, sec := range result.Sections {
					got := sec.Variants[0].Text
					if n := estimateTokens(got); n > cfg.maxTokens {
						t.Errorf("chunk %d has %d tokens, want <= %d", i, n, cfg.maxTokens)
					}
					for _, w := range strings.Fields(got) {
						if !sourceWords[w] {
							t.Errorf("chunk %d contains %q, which is not a source word", i, w)
						}
					}
				}
			})
		}
	}
}

func TestRecursiveChunkerOverlapCarriesPreviousTail(t *testing.T) {
	words := make([]string, 400)
	for i := range words {
		words[i] = fmt.Sprintf("word%d", i)
	}
	c := NewRecursive(&Config{MaxTokens: 100, Overlap: 20, Separators: []string{" "}})
	result, err := c.Chunk(context.Background(), makeEdgeDoc(strings.Join(words, " ")))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Sections) < 2 {
		t.Fatalf("expected multiple sections, got %d", len(result.Sections))
	}
	first := strings.Fields(result.Sections[0].Variants[0].Text)
	second := strings.Fields(result.Sections[1].Variants[0].Text)
	lastOfFirst := first[len(first)-1]
	found := false
	for _, w := range second {
		if w == lastOfFirst {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("second chunk should repeat %q from the end of the first chunk", lastOfFirst)
	}
}

func textVariant(uuid, sectionUUID, text string) types.ContentVariant {
	return types.ContentVariant{UUID: uuid, SectionUUID: sectionUUID, ContentType: types.ContentText, MIMEType: "text/plain", Text: text}
}

func imageVariant(uuid, sectionUUID string) types.ContentVariant {
	return types.ContentVariant{UUID: uuid, SectionUUID: sectionUUID, ContentType: types.ContentImage, MIMEType: "image/png", Data: []byte{1, 2, 3}}
}

// fixedEmbedder returns orthogonal vectors for alternating inputs so the
// semantic chunker splits between consecutive sentences.
type fixedEmbedder struct{ short bool }

func (fixedEmbedder) Register(types.ContentType, types.VariantEmbedder) {}
func (e fixedEmbedder) Embed(_ context.Context, variants []types.ContentVariant) ([][]float32, error) {
	n := len(variants)
	if e.short {
		n--
	}
	out := make([][]float32, n)
	for i := range out {
		out[i] = []float32{float32(i % 2), float32((i + 1) % 2)}
	}
	return out, nil
}

func TestChunkersEmitMultiVariantSectionsOnce(t *testing.T) {
	long := strings.Repeat("Sentence number one is here. ", 60)
	recursive := NewRecursive(&Config{MaxTokens: 40, Overlap: 0, Separators: []string{". ", " "}})
	semantic := NewSemantic(fixedEmbedder{}, &SemanticConfig{Threshold: 0.5, MinTokens: 20, MaxTokens: 40})

	tests := []struct {
		name        string
		variants    []types.ContentVariant
		wantSecs    int // exact section count; 0 means "more than one"
		wantImages  int
		wantShort   int // short text variants that must survive unchanged
		maxTokens   int
		chunkerName string
	}{
		{name: "short text and image", variants: []types.ContentVariant{textVariant("v1", "s1", "short text"), imageVariant("v2", "s1")}, wantSecs: 1, wantImages: 1, wantShort: 1},
		{name: "two short texts", variants: []types.ContentVariant{textVariant("v1", "s1", "first short"), textVariant("v2", "s1", "second short")}, wantSecs: 1, wantShort: 2},
		{name: "long text and image", variants: []types.ContentVariant{textVariant("v1", "s1", long), imageVariant("v2", "s1")}, wantImages: 1},
		{name: "image only", variants: []types.ContentVariant{imageVariant("v2", "s1")}, wantSecs: 1, wantImages: 1},
	}
	chunkers := map[string]types.Chunker{"recursive": recursive, "semantic": semantic}

	for chunkerName, ch := range chunkers {
		for _, tt := range tests {
			t.Run(chunkerName+"/"+tt.name, func(t *testing.T) {
				doc := &types.Document{UUID: "d1", Sections: []types.Section{{UUID: "s1", DocumentUUID: "d1", Variants: tt.variants}}}
				result, err := ch.Chunk(context.Background(), doc)
				if err != nil {
					t.Fatal(err)
				}
				if tt.wantSecs > 0 && len(result.Sections) != tt.wantSecs {
					t.Fatalf("got %d sections, want %d", len(result.Sections), tt.wantSecs)
				}
				if tt.wantSecs == 0 && len(result.Sections) < 2 {
					t.Fatalf("got %d sections, want the long text split", len(result.Sections))
				}
				if result.Sections[0].UUID != "s1" {
					t.Errorf("first section UUID = %q, want the original s1", result.Sections[0].UUID)
				}

				secUUIDs := make(map[string]bool)
				varUUIDs := make(map[string]bool)
				images, short := 0, 0
				for i, sec := range result.Sections {
					if sec.Index != i {
						t.Errorf("section %d has index %d", i, sec.Index)
					}
					if secUUIDs[sec.UUID] {
						t.Errorf("duplicate section UUID %q", sec.UUID)
					}
					secUUIDs[sec.UUID] = true
					for _, v := range sec.Variants {
						if varUUIDs[v.UUID] {
							t.Errorf("duplicate variant UUID %q", v.UUID)
						}
						varUUIDs[v.UUID] = true
						if v.SectionUUID != sec.UUID {
							t.Errorf("variant %q points at section %q, lives in %q", v.UUID, v.SectionUUID, sec.UUID)
						}
						switch {
						case v.ContentType == types.ContentImage:
							images++
						case v.Text == long:
							t.Error("long text was emitted unchunked")
						case v.UUID == "v1" || v.UUID == "v2":
							short++
						}
						if v.ContentType == types.ContentText && estimateTokens(v.Text) > 40 {
							t.Errorf("text variant has %d tokens, want <= 40", estimateTokens(v.Text))
						}
					}
				}
				if images != tt.wantImages {
					t.Errorf("got %d image variants, want %d", images, tt.wantImages)
				}
				if short != tt.wantShort {
					t.Errorf("got %d unchanged short text variants, want %d", short, tt.wantShort)
				}
			})
		}
	}
}

func TestSemanticChunkerRejectsShortEmbeddingOutput(t *testing.T) {
	c := NewSemantic(fixedEmbedder{short: true}, &SemanticConfig{Threshold: 0.5, MinTokens: 1, MaxTokens: 512})
	_, err := c.Chunk(context.Background(), makeEdgeDoc("One sentence here. Another sentence there. A third one."))
	if err == nil || !strings.Contains(err.Error(), types.ErrEmbeddingShape.Error()) {
		t.Fatalf("expected ErrEmbeddingShape, got %v", err)
	}
}
