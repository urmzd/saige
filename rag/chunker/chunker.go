// Package chunker provides chunking strategies for splitting documents into smaller sections.
package chunker

import (
	"context"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"github.com/urmzd/saige/rag/tokenizer"
	"github.com/urmzd/saige/rag/types"
)

// Config holds recursive chunker parameters.
type Config struct {
	// MaxTokens caps every emitted chunk, overlap included.
	MaxTokens int
	// Overlap is the approximate number of tokens repeated from the end of
	// the previous chunk. Values above MaxTokens/2 are clamped to MaxTokens/2.
	Overlap    int
	Separators []string
}

// DefaultConfig returns standard recursive chunker parameters.
func DefaultConfig() *Config {
	return &Config{
		MaxTokens:  512,
		Overlap:    50,
		Separators: []string{"\n\n", "\n", ". ", " "},
	}
}

// RecursiveChunker splits sections by trying separators in order, recursing with the next
// separator if any chunk exceeds MaxTokens.
type RecursiveChunker struct {
	cfg Config
}

// NewRecursive creates a recursive text chunker. If cfg is nil, defaults are used.
func NewRecursive(cfg *Config) *RecursiveChunker {
	if cfg == nil {
		cfg = DefaultConfig()
	}

	normalized := *cfg
	if normalized.MaxTokens <= 0 {
		normalized.MaxTokens = DefaultConfig().MaxTokens
	}
	if normalized.Overlap < 0 {
		normalized.Overlap = 0
	}
	// Chunks are split at MaxTokens minus Overlap, so a larger overlap would
	// leave too little room for new text and force splits inside words.
	if normalized.Overlap > normalized.MaxTokens/2 {
		normalized.Overlap = normalized.MaxTokens / 2
	}
	if normalized.Separators == nil {
		normalized.Separators = DefaultConfig().Separators
	} else {
		normalized.Separators = append([]string(nil), normalized.Separators...)
	}

	return &RecursiveChunker{cfg: normalized}
}

func estimateTokens(text string) int {
	return tokenizer.CountTokens(text)
}

// Chunk splits long sections in the document into smaller ones.
func (c *RecursiveChunker) Chunk(_ context.Context, doc *types.Document) (*types.Document, error) {
	return chunkDocument(doc, func(v types.ContentVariant) bool {
		return estimateTokens(v.Text) > c.cfg.MaxTokens
	}, func(text string) ([]string, error) {
		return c.split(text), nil
	})
}

// chunkDocument emits each section of doc exactly once, splitting only the
// text variants for which needsSplit reports true. Non-text variants and text
// variants that already fit stay on the first emitted section, which keeps
// the original section UUID; every further chunk becomes a new section with
// fresh UUIDs. Section indexes are renumbered in output order.
func chunkDocument(
	doc *types.Document,
	needsSplit func(types.ContentVariant) bool,
	split func(string) ([]string, error),
) (*types.Document, error) {
	var newSections []types.Section

	for _, sec := range doc.Sections {
		var kept, long []types.ContentVariant
		for _, v := range sec.Variants {
			if v.ContentType == types.ContentText && needsSplit(v) {
				long = append(long, v)
			} else {
				kept = append(kept, v)
			}
		}

		if len(long) == 0 {
			sec.Index = len(newSections)
			newSections = append(newSections, sec)
			continue
		}

		first := sec
		first.Variants = kept
		firstHasChunk := false
		var rest []types.Section

		for _, v := range long {
			chunks, err := split(v.Text)
			if err != nil {
				return nil, err
			}
			for _, chunk := range chunks {
				chunk = strings.TrimSpace(chunk)
				if chunk == "" {
					continue
				}
				chunkVariant := types.ContentVariant{
					UUID:        uuid.New().String(),
					ContentType: v.ContentType,
					MIMEType:    v.MIMEType,
					Text:        chunk,
					Metadata:    v.Metadata,
				}
				if !firstHasChunk {
					chunkVariant.SectionUUID = first.UUID
					first.Variants = append(first.Variants, chunkVariant)
					firstHasChunk = true
					continue
				}
				secUUID := uuid.New().String()
				chunkVariant.SectionUUID = secUUID
				rest = append(rest, types.Section{
					UUID:         secUUID,
					DocumentUUID: doc.UUID,
					Heading:      sec.Heading,
					Variants:     []types.ContentVariant{chunkVariant},
				})
			}
		}

		if len(first.Variants) > 0 {
			first.Index = len(newSections)
			newSections = append(newSections, first)
		}
		for _, r := range rest {
			r.Index = len(newSections)
			newSections = append(newSections, r)
		}
	}

	result := *doc
	result.Sections = newSections
	return &result, nil
}

// split chunks text so every returned chunk, overlap included, stays within
// MaxTokens. Splitting runs at a budget of MaxTokens minus Overlap so the
// overlap prefix fits without pushing a chunk past the limit.
func (c *RecursiveChunker) split(text string) []string {
	budget := c.cfg.MaxTokens - c.cfg.Overlap
	if budget < 1 {
		budget = 1
	}
	raw := c.splitRecursive(text, 0, budget)
	chunks := make([]string, 0, len(raw))
	for _, chunk := range raw {
		if chunk = strings.TrimSpace(chunk); chunk != "" {
			chunks = append(chunks, chunk)
		}
	}
	return c.applyOverlap(chunks)
}

func (c *RecursiveChunker) splitRecursive(text string, sepIdx, budget int) []string {
	if estimateTokens(text) <= budget {
		return []string{text}
	}

	if sepIdx >= len(c.cfg.Separators) {
		// Leaf: hard split at the budget without cutting through UTF-8 runes.
		return hardSplit(text, budget)
	}

	sep := c.cfg.Separators[sepIdx]
	parts := strings.Split(text, sep)
	if len(parts) <= 1 {
		return c.splitRecursive(text, sepIdx+1, budget)
	}

	var chunks []string
	current := ""

	for i, part := range parts {
		candidate := current
		if candidate != "" {
			candidate += sep
		}
		candidate += part

		if estimateTokens(candidate) > budget && current != "" {
			chunks = append(chunks, current)
			current = part
		} else {
			current = candidate
		}

		if i == len(parts)-1 && current != "" {
			chunks = append(chunks, current)
		}
	}

	// Recurse on any chunks that are still too large.
	var result []string
	for _, chunk := range chunks {
		if estimateTokens(chunk) > budget {
			result = append(result, c.splitRecursive(chunk, sepIdx+1, budget)...)
		} else {
			result = append(result, chunk)
		}
	}

	return result
}

func hardSplit(text string, budget int) []string {
	var chunks []string
	for estimateTokens(text) > budget {
		boundaries := runeBoundaries(text)
		// Binary search for the largest rune-boundary split point within budget.
		lo, hi := 0, len(boundaries)-1
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if estimateTokens(text[:boundaries[mid]]) <= budget {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		split := boundaries[lo]
		if split == 0 {
			split = boundaries[1] // ensure progress, even for a single oversized rune
		}
		chunks = append(chunks, text[:split])
		text = text[split:]
	}
	if text != "" {
		chunks = append(chunks, text)
	}
	return chunks
}

// applyOverlap prefixes each chunk after the first with the tail of the
// previous chunk. The tail starts at a word boundary and is joined with a
// space, so a seam never fuses two words. The tail is shortened word by word
// until the combined chunk fits MaxTokens; when no word-aligned tail fits, the
// chunk is emitted without overlap.
func (c *RecursiveChunker) applyOverlap(chunks []string) []string {
	if c.cfg.Overlap <= 0 || len(chunks) <= 1 {
		return chunks
	}

	result := make([]string, len(chunks))
	result[0] = chunks[0]

	for i := 1; i < len(chunks); i++ {
		result[i] = chunks[i]
		prev := chunks[i-1]
		starts := wordStarts(prev)
		// Binary search for the earliest word start whose suffix fits Overlap.
		// Suffix token counts shrink as the start moves right.
		lo, hi := 0, len(starts)
		for lo < hi {
			mid := (lo + hi) / 2
			if estimateTokens(prev[starts[mid]:]) <= c.cfg.Overlap {
				hi = mid
			} else {
				lo = mid + 1
			}
		}
		for k := lo; k < len(starts); k++ {
			candidate := prev[starts[k]:] + " " + chunks[i]
			if estimateTokens(candidate) <= c.cfg.MaxTokens {
				result[i] = candidate
				break
			}
		}
	}

	return result
}

// wordStarts returns the byte offsets in text where a word begins: offset 0
// when text starts with a non-space rune, and every non-space rune that
// follows whitespace.
func wordStarts(text string) []int {
	var starts []int
	prevSpace := true
	for i, r := range text {
		space := unicode.IsSpace(r)
		if !space && prevSpace {
			starts = append(starts, i)
		}
		prevSpace = space
	}
	return starts
}

func runeBoundaries(text string) []int {
	boundaries := []int{0}
	for i := range text {
		if i != 0 {
			boundaries = append(boundaries, i)
		}
	}
	if boundaries[len(boundaries)-1] != len(text) {
		boundaries = append(boundaries, len(text))
	}
	return boundaries
}
