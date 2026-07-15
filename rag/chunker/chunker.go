// Package chunker provides chunking strategies for splitting documents into smaller sections.
package chunker

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/urmzd/saige/rag/tokenizer"
	"github.com/urmzd/saige/rag/types"
)

// Config holds recursive chunker parameters.
type Config struct {
	MaxTokens  int
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
	var newSections []types.Section
	idx := 0

	for _, sec := range doc.Sections {
		for _, v := range sec.Variants {
			if v.ContentType != types.ContentText || estimateTokens(v.Text) <= c.cfg.MaxTokens {
				sec.Index = idx
				newSections = append(newSections, sec)
				idx++
				continue
			}

			chunks := c.splitRecursive(v.Text, 0)
			chunks = c.applyOverlap(chunks)

			for _, chunk := range chunks {
				chunk = strings.TrimSpace(chunk)
				if chunk == "" {
					continue
				}
				secUUID := uuid.New().String()
				varUUID := uuid.New().String()
				newSections = append(newSections, types.Section{
					UUID:         secUUID,
					DocumentUUID: doc.UUID,
					Index:        idx,
					Heading:      sec.Heading,
					Variants: []types.ContentVariant{{
						UUID:        varUUID,
						SectionUUID: secUUID,
						ContentType: v.ContentType,
						MIMEType:    v.MIMEType,
						Text:        chunk,
						Metadata:    v.Metadata,
					}},
				})
				idx++
			}
		}
	}

	result := *doc
	result.Sections = newSections
	return &result, nil
}

func (c *RecursiveChunker) splitRecursive(text string, sepIdx int) []string {
	if estimateTokens(text) <= c.cfg.MaxTokens {
		return []string{text}
	}

	if sepIdx >= len(c.cfg.Separators) {
		// Leaf: hard split at MaxTokens without cutting through UTF-8 runes.
		return c.hardSplit(text)
	}

	sep := c.cfg.Separators[sepIdx]
	parts := strings.Split(text, sep)
	if len(parts) <= 1 {
		return c.splitRecursive(text, sepIdx+1)
	}

	var chunks []string
	current := ""

	for i, part := range parts {
		candidate := current
		if candidate != "" {
			candidate += sep
		}
		candidate += part

		if estimateTokens(candidate) > c.cfg.MaxTokens && current != "" {
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
		if estimateTokens(chunk) > c.cfg.MaxTokens {
			result = append(result, c.splitRecursive(chunk, sepIdx+1)...)
		} else {
			result = append(result, chunk)
		}
	}

	return result
}

func (c *RecursiveChunker) hardSplit(text string) []string {
	var chunks []string
	for estimateTokens(text) > c.cfg.MaxTokens {
		boundaries := runeBoundaries(text)
		// Binary search for the largest rune-boundary split point under MaxTokens.
		lo, hi := 0, len(boundaries)-1
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if estimateTokens(text[:boundaries[mid]]) <= c.cfg.MaxTokens {
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

func (c *RecursiveChunker) applyOverlap(chunks []string) []string {
	if c.cfg.Overlap <= 0 || len(chunks) <= 1 {
		return chunks
	}

	result := make([]string, len(chunks))
	result[0] = chunks[0]

	for i := 1; i < len(chunks); i++ {
		prev := chunks[i-1]
		// Find the suffix of prev that is approximately Overlap tokens.
		overlapText := prev
		if estimateTokens(prev) > c.cfg.Overlap {
			boundaries := runeBoundaries(prev)
			// Binary search for a rune-boundary start yielding Overlap tokens from suffix.
			lo, hi := 0, len(boundaries)-1
			for lo < hi {
				mid := (lo + hi) / 2
				if estimateTokens(prev[boundaries[mid]:]) > c.cfg.Overlap {
					lo = mid + 1
				} else {
					hi = mid
				}
			}
			start := boundaries[lo]
			if start == len(prev) && len(boundaries) > 1 {
				start = boundaries[len(boundaries)-2]
			}
			overlapText = prev[start:]
		}
		result[i] = overlapText + chunks[i]
	}

	return result
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
