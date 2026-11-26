// Package contextassembler provides context assembly strategies for RAG pipelines.
package contextassembler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

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

// WithMaxTokens returns a copy of a that uses maxTokens as its budget.
func (a *CompressingAssembler) WithMaxTokens(maxTokens int) types.ContextAssembler {
	c := *a
	c.MaxTokens = maxTokens
	return &c
}

// Assemble compresses each hit's text via the LLM and builds context with
// citations. Hits are compressed in parallel, then the token budget is applied
// in rank order, skipping hits that do not fit.
//
// A hit whose compression call fails keeps its original text, so one flaky
// call does not lose the whole context. The result then comes with an error
// wrapping types.ErrPartialSearch that lists the failures. Only a done
// context fails the call outright.
func (a *CompressingAssembler) Assemble(ctx context.Context, query string, hits []types.SearchHit) (*types.AssembledContext, error) {
	// Repeated passages would be compressed and cited twice.
	hits = uniqueByText(hits)

	texts := make([]string, len(hits))
	errs := make([]error, len(hits))
	var wg sync.WaitGroup
	for i, hit := range hits {
		prompt := renderPrompt(compressionTmpl, map[string]any{"Query": query, "Text": hit.Variant.Text})
		wg.Go(func() {
			text, err := a.LLM.Generate(ctx, prompt)
			if err != nil {
				texts[i] = hit.Variant.Text
				errs[i] = fmt.Errorf("compress hit %d: %w", i, err)
				return
			}
			if irrelevant(text) {
				return
			}
			texts[i] = strings.TrimSpace(text)
		})
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	result := build(query, hits, texts, a.MaxTokens)
	if err := errors.Join(errs...); err != nil {
		return result, fmt.Errorf("%w: %w", types.ErrPartialSearch, err)
	}
	return result, nil
}

// irrelevant reports whether a compression reply says the passage has
// nothing relevant: empty, or "N/A" in any case with stray punctuation.
func irrelevant(reply string) bool {
	r := strings.ToUpper(strings.Trim(strings.TrimSpace(reply), ".!\"'`*"))
	return r == "" || r == "N/A" || r == "NA"
}
