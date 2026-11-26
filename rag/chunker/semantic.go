package chunker

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

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
		// No boundary to measure, but the result must still respect MaxTokens.
		return c.enforceTokenLimits(sentences), nil
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

	// Determine where to split. A configured threshold splits where
	// similarity drops strictly below it. The auto threshold is the
	// interpolated 10th percentile and splits at or below it, unless every
	// similarity is equal, in which case there is no topic change to split on.
	isBreak := func(sim float64) bool { return sim < c.cfg.Threshold }
	if c.cfg.Threshold == 0 {
		threshold := percentile(sims, 10)
		if threshold < slices.Max(sims) {
			isBreak = func(sim float64) bool { return sim <= threshold }
		} else {
			isBreak = func(float64) bool { return false }
		}
	}

	// Split where similarity drops below threshold.
	var chunks []string
	var current strings.Builder
	current.WriteString(sentences[0])
	currentTokens := estimateTokens(sentences[0])
	for i := 0; i < len(sims); i++ {
		next := sentences[i+1]
		if isBreak(sims[i]) && currentTokens >= c.cfg.MinTokens {
			chunks = append(chunks, current.String())
			current.Reset()
			currentTokens = 0
		} else {
			current.WriteByte(' ')
		}
		current.WriteString(next)
		currentTokens += estimateTokens(next)
	}
	if current.Len() > 0 {
		chunks = append(chunks, current.String())
	}

	// Merge small chunks and split large ones.
	return c.enforceTokenLimits(chunks), nil
}

// isSentenceEnd reports whether r ends a sentence regardless of what follows
// it: full-width CJK terminators and line breaks.
func isSentenceEnd(r rune) bool {
	switch r {
	case '\n', '。', '！', '？', '｡':
		return true
	}
	return false
}

// splitIntoSentences splits text after ASCII terminators (. ! ?) followed by
// whitespace or the end of text, after full-width CJK terminators, and at line
// breaks, so code, lists and CJK prose still yield several units.
func splitIntoSentences(text string) []string {
	var sentences []string
	var current strings.Builder
	flush := func() {
		if s := strings.TrimSpace(current.String()); s != "" {
			sentences = append(sentences, s)
		}
		current.Reset()
	}
	for i, r := range text {
		current.WriteRune(r)
		next := i + utf8.RuneLen(r)
		switch {
		case isSentenceEnd(r):
			flush()
		case (r == '.' || r == '!' || r == '?') &&
			(next >= len(text) || text[next] == ' ' || text[next] == '\t' || text[next] == '\r'):
			flush()
		}
	}
	flush()
	if len(sentences) == 0 && strings.TrimSpace(text) != "" {
		return []string{strings.TrimSpace(text)}
	}
	return sentences
}

func (c *SemanticChunker) enforceTokenLimits(chunks []string) []string {
	// Merge small chunks.
	var merged []string
	var current strings.Builder
	currentTokens := 0
	for _, chunk := range chunks {
		if current.Len() == 0 {
			current.WriteString(chunk)
			currentTokens = estimateTokens(chunk)
			continue
		}
		if currentTokens < c.cfg.MinTokens {
			current.WriteByte(' ')
			current.WriteString(chunk)
			currentTokens += estimateTokens(chunk)
		} else {
			merged = append(merged, current.String())
			current.Reset()
			current.WriteString(chunk)
			currentTokens = estimateTokens(chunk)
		}
	}
	if current.Len() > 0 {
		merged = append(merged, current.String())
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

// percentile returns the p-th percentile of vals with linear interpolation
// between closest ranks, so small samples do not collapse to the minimum.
func percentile(vals []float64, p float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := make([]float64, len(vals))
	copy(sorted, vals)
	sort.Float64s(sorted)
	rank := p / 100.0 * float64(len(sorted)-1)
	if rank <= 0 {
		return sorted[0]
	}
	if rank >= float64(len(sorted)-1) {
		return sorted[len(sorted)-1]
	}
	lo := int(math.Floor(rank))
	frac := rank - float64(lo)
	return sorted[lo] + frac*(sorted[lo+1]-sorted[lo])
}
