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
//
// MaxTokens bounds the whole prompt: header, citation markers and source
// lines included. A hit that does not fit is skipped and later, smaller hits
// are still tried, so one oversized top hit cannot empty the context.
type DefaultAssembler struct {
	MaxTokens int
}

// WithMaxTokens returns a copy of a that uses maxTokens as its budget.
func (a *DefaultAssembler) WithMaxTokens(maxTokens int) types.ContextAssembler {
	c := *a
	c.MaxTokens = maxTokens
	return &c
}

// Assemble implements types.ContextAssembler.
func (a *DefaultAssembler) Assemble(_ context.Context, query string, hits []types.SearchHit) (*types.AssembledContext, error) {
	hits = uniqueByText(hits)
	texts := make([]string, len(hits))
	for i, hit := range hits {
		texts[i] = hit.Variant.Text
	}
	return build(query, hits, texts, a.MaxTokens), nil
}

// build formats texts[i] (the text shown for hits[i]) into a cited prompt
// within maxTokens. Empty texts are skipped. maxTokens <= 0 means no limit.
func build(query string, hits []types.SearchHit, texts []string, maxTokens int) *types.AssembledContext {
	header := fmt.Sprintf("Context for query %q:\n\n", query)
	const sep = "\n\n"
	used := tokenizer.CountTokens(header)

	var blocks []types.ContextBlock
	var parts []string
	for i, text := range texts {
		if strings.TrimSpace(text) == "" {
			continue
		}
		citation := fmt.Sprintf("[%d]", len(blocks)+1)
		source := hits[i].Provenance.SourceURI
		if source == "" {
			source = hits[i].Provenance.DocumentTitle
		}
		part := fmt.Sprintf("%s %s (Source: %s)", citation, text, source)
		cost := tokenizer.CountTokens(part)
		if len(parts) > 0 {
			cost += tokenizer.CountTokens(sep)
		}
		if maxTokens > 0 && used+cost > maxTokens {
			continue
		}
		used += cost
		blocks = append(blocks, types.ContextBlock{
			Text:       text,
			Citation:   citation,
			Provenance: hits[i].Provenance,
		})
		parts = append(parts, part)
	}

	tokens := 0
	if len(blocks) > 0 {
		tokens = used
	}
	return &types.AssembledContext{
		Prompt:     header + strings.Join(parts, sep),
		Blocks:     blocks,
		TokenCount: tokens,
	}
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
