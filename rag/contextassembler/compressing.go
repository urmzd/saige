// Package contextassembler provides context assembly strategies for RAG pipelines.
package contextassembler

import (
	"context"
	"fmt"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/urmzd/saige/rag/tokenizer"
	"github.com/urmzd/saige/rag/types"
)

// CompressingAssembler uses an LLM to extract query-relevant sentences from each hit
// before assembling context with citations.
type CompressingAssembler struct {
	LLM       types.LLM
	MaxTokens int
}

// NewCompressing creates a compressing context assembler.
func NewCompressing(llm types.LLM, maxTokens int) *CompressingAssembler {
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	return &CompressingAssembler{LLM: llm, MaxTokens: maxTokens}
}

// Assemble compresses each hit's text via the LLM and builds context with citations.
// Phase 1 compresses all hits in parallel; phase 2 applies the token budget sequentially.
func (a *CompressingAssembler) Assemble(ctx context.Context, query string, hits []types.SearchHit) (*types.AssembledContext, error) {
	// Phase 1: Parallel LLM compression.
	compressedTexts := make([]string, len(hits))
	g, gctx := errgroup.WithContext(ctx)

	for i, hit := range hits {
		prompt := renderPrompt(compressionTmpl, map[string]any{"Query": query, "Text": hit.Variant.Text})
		g.Go(func() error {
			text, err := a.LLM.Generate(gctx, prompt)
			if err != nil {
				return fmt.Errorf("compress hit %d: %w", i, err)
			}
			compressedTexts[i] = text
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	// Phase 2: Sequential token budget assembly.
	var blocks []types.ContextBlock
	var parts []string
	tokenCount := 0

	for i, compressed := range compressedTexts {
		if compressed == "" || compressed == "N/A" {
			continue
		}

		tokens := tokenizer.CountTokens(compressed)
		if a.MaxTokens > 0 && tokenCount+tokens > a.MaxTokens {
			break
		}
		tokenCount += tokens

		citation := fmt.Sprintf("[%d]", len(blocks)+1)
		blocks = append(blocks, types.ContextBlock{
			Text:       compressed,
			Citation:   citation,
			Provenance: hits[i].Provenance,
		})

		source := hits[i].Provenance.SourceURI
		if source == "" {
			source = hits[i].Provenance.DocumentTitle
		}
		parts = append(parts, fmt.Sprintf("%s %s (Source: %s)", citation, compressed, source))
	}

	promptText := fmt.Sprintf("Context for query %q:\n\n%s", query, strings.Join(parts, "\n\n"))

	return &types.AssembledContext{
		Prompt:     promptText,
		Blocks:     blocks,
		TokenCount: tokenCount,
	}, nil
}
