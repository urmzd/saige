package chunker

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/urmzd/saige/rag/types"
)

// SemanticConfig holds semantic chunker parameters.
type SemanticConfig struct {
	Threshold float64 // Similarity threshold; 0 means auto (10th percentile).
	MinTokens int
	MaxTokens int
}

// DefaultSemanticConfig returns standard semantic chunker parameters.
func DefaultSemanticConfig() *SemanticConfig {
	return &SemanticConfig{
		Threshold: 0, // auto
		MinTokens: 50,
		MaxTokens: 512,
	}
}

// SemanticChunker splits text at points where consecutive sentence embeddings
// drop below a similarity threshold.
type SemanticChunker struct {
	cfg       SemanticConfig
	embedders types.EmbedderRegistry
}

// NewSemantic creates a semantic chunker. If cfg is nil, defaults are used.
func NewSemantic(embedders types.EmbedderRegistry, cfg *SemanticConfig) *SemanticChunker {
	if cfg == nil {
		cfg = DefaultSemanticConfig()
	}
	return &SemanticChunker{cfg: *cfg, embedders: embedders}
}

// Chunk splits document sections using semantic similarity between sentences.
func (c *SemanticChunker) Chunk(ctx context.Context, doc *types.Document) (*types.Document, error) {
	return chunkDocument(doc, func(v types.ContentVariant) bool {
		return estimateTokens(v.Text) > c.cfg.MinTokens
	}, func(text string) ([]string, error) {
		return c.splitSemantic(ctx, text)
	})
}

func (c *SemanticChunker) splitSemantic(ctx context.Context, text string) ([]string, error) {
	sentences := splitIntoSentences(text)
	if len(sentences) <= 1 {
		return sentences, nil
	}

	// Embed each sentence.
	variants := make([]types.ContentVariant, len(sentences))
	for i, s := range sentences {
		variants[i] = types.ContentVariant{
			ContentType: types.ContentText,
			Text:        s,
		}
	}
	embeddings, err := c.embedders.Embed(ctx, variants)
	if err != nil {
		return nil, err
	}
	if len(embeddings) != len(sentences) {
		return nil, fmt.Errorf("semantic chunker: %w: got %d vectors for %d sentences",
			types.ErrEmbeddingShape, len(embeddings), len(sentences))
	}

	// Compute similarities between consecutive sentences.
	sims := make([]float64, len(sentences)-1)
	for i := 0; i < len(sentences)-1; i++ {
		sims[i] = cosineSimilarity(embeddings[i], embeddings[i+1])
	}

	// Determine threshold.
	threshold := c.cfg.Threshold
	if threshold == 0 {
		threshold = percentile(sims, 10)
	}

	// Split where similarity drops below threshold.
	var chunks []string
	current := sentences[0]
	for i := 0; i < len(sims); i++ {
		if sims[i] < threshold && estimateTokens(current) >= c.cfg.MinTokens {
			chunks = append(chunks, current)
			current = sentences[i+1]
		} else {
			current += " " + sentences[i+1]
		}
	}
	if current != "" {
		chunks = append(chunks, current)
	}

	// Merge small chunks and split large ones.
	chunks = c.enforceTokenLimits(chunks)

	return chunks, nil
}

func splitIntoSentences(text string) []string {
	// Split on sentence-ending punctuation followed by space or end.
	var sentences []string
	current := ""
	for i, r := range text {
		current += string(r)
		if (r == '.' || r == '!' || r == '?') && (i+1 >= len(text) || text[i+1] == ' ' || text[i+1] == '\n') {
			sentences = append(sentences, strings.TrimSpace(current))
			current = ""
		}
	}
	if strings.TrimSpace(current) != "" {
		sentences = append(sentences, strings.TrimSpace(current))
	}
	if len(sentences) == 0 {
		return []string{text}
	}
	return sentences
}

func (c *SemanticChunker) enforceTokenLimits(chunks []string) []string {
	// Merge small chunks.
	var merged []string
	current := ""
	for _, chunk := range chunks {
		if current == "" {
			current = chunk
			continue
		}
		combined := current + " " + chunk
		if estimateTokens(current) < c.cfg.MinTokens {
			current = combined
		} else {
			merged = append(merged, current)
			current = chunk
		}
	}
	if current != "" {
		merged = append(merged, current)
	}

	// Split large chunks using recursive chunker as fallback.
	var result []string
	rc := NewRecursive(&Config{MaxTokens: c.cfg.MaxTokens, Overlap: 0, Separators: []string{". ", " "}})
	for _, chunk := range merged {
		if estimateTokens(chunk) > c.cfg.MaxTokens {
			result = append(result, rc.split(chunk)...)
		} else {
			result = append(result, chunk)
		}
	}

	return result
}

func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	return dot / denom
}

func percentile(vals []float64, p float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := make([]float64, len(vals))
	copy(sorted, vals)
	sort.Float64s(sorted)
	idx := int(math.Floor(p / 100.0 * float64(len(sorted))))
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
