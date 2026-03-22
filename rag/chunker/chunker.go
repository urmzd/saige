// Package chunker provides chunking strategies for splitting documents into smaller sections.
package chunker

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/urmzd/graph-agent-dev-kit/rag/ragtypes"
	"github.com/urmzd/graph-agent-dev-kit/rag/tokenizer"
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
	return &RecursiveChunker{cfg: *cfg}
}

func estimateTokens(text string) int {
	return tokenizer.CountTokens(text)
}

// Chunk splits long sections in the document into smaller ones.
func (c *RecursiveChunker) Chunk(_ context.Context, doc *ragtypes.Document) (*ragtypes.Document, error) {
	var newSections []ragtypes.Section
	idx := 0

	for _, sec := range doc.Sections {
		for _, v := range sec.Variants {
			if v.ContentType != ragtypes.ContentText || estimateTokens(v.Text) <= c.cfg.MaxTokens {
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
				newSections = append(newSections, ragtypes.Section{
					UUID:         secUUID,
					DocumentUUID: doc.UUID,
					Index:        idx,
					Heading:      sec.Heading,
					Variants: []ragtypes.ContentVariant{{
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
		// Leaf: hard split at MaxTokens character boundaries.
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
		// Binary search for the split point that yields MaxTokens tokens.
		lo, hi := 0, len(text)
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if estimateTokens(text[:mid]) <= c.cfg.MaxTokens {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		if lo == 0 {
			lo = 1 // ensure progress
		}
		chunks = append(chunks, text[:lo])
		text = text[lo:]
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
			// Binary search for start position yielding Overlap tokens from suffix.
			lo, hi := 0, len(prev)
			for lo < hi {
				mid := (lo + hi) / 2
				if estimateTokens(prev[mid:]) > c.cfg.Overlap {
					lo = mid + 1
				} else {
					hi = mid
				}
			}
			overlapText = prev[lo:]
		}
		result[i] = overlapText + chunks[i]
	}

	return result
}
