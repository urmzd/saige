package contextassembler

import (
	"context"
	"fmt"
	"strings"

	"github.com/urmzd/saige/rag/tokenizer"
	"github.com/urmzd/saige/rag/types"
)

// DefaultAssembler builds context with numbered citations from exact source text.
// Hits whose trimmed text repeats an earlier hit's text produce no second
// block, so expanded parent sections and duplicate passages do not spend the
// token budget twice.
type DefaultAssembler struct {
	MaxTokens int
}

// Assemble implements types.ContextAssembler.
func (a *DefaultAssembler) Assemble(_ context.Context, query string, hits []types.SearchHit) (*types.AssembledContext, error) {
	var blocks []types.ContextBlock
	var parts []string
	tokenCount := 0

	for _, hit := range uniqueByText(hits) {
		citation := fmt.Sprintf("[%d]", len(blocks)+1)
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

// uniqueByText returns hits without those whose trimmed text equals the text
// of an earlier hit. Hits with empty text are kept. Order is preserved.
func uniqueByText(hits []types.SearchHit) []types.SearchHit {
	seen := make(map[string]bool, len(hits))
	out := make([]types.SearchHit, 0, len(hits))
	for _, hit := range hits {
		text := strings.TrimSpace(hit.Variant.Text)
		if text != "" {
			if seen[text] {
				continue
			}
			seen[text] = true
		}
		out = append(out, hit)
	}
	return out
}
