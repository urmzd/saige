package contextassembler

import (
	"context"
	"fmt"
	"strings"

	"github.com/urmzd/saige/rag/tokenizer"
	"github.com/urmzd/saige/rag/types"
)

// DefaultAssembler builds context with numbered citations from exact source text.
type DefaultAssembler struct {
	MaxTokens int
}

func (a *DefaultAssembler) Assemble(_ context.Context, query string, hits []types.SearchHit) (*types.AssembledContext, error) {
	var blocks []types.ContextBlock
	var parts []string
	tokenCount := 0

	for i, hit := range hits {
		citation := fmt.Sprintf("[%d]", i+1)
		text := hit.Variant.Text

		tokens := tokenizer.CountTokens(text)
		if a.MaxTokens > 0 && tokenCount+tokens > a.MaxTokens {
			break
		}
		tokenCount += tokens

		blocks = append(blocks, types.ContextBlock{
			Text:       text,
			Citation:   citation,
			Provenance: hit.Provenance,
		})

		source := hit.Provenance.SourceURI
		if source == "" {
			source = hit.Provenance.DocumentTitle
		}
		parts = append(parts, fmt.Sprintf("%s %s (Source: %s)", citation, text, source))
	}

	prompt := fmt.Sprintf("Context for query %q:\n\n%s", query, strings.Join(parts, "\n\n"))

	return &types.AssembledContext{
		Prompt:     prompt,
		Blocks:     blocks,
		TokenCount: tokenCount,
	}, nil
}
