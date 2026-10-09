// Package hyde implements Hypothetical Document Embeddings (HyDE) query transformation.
package hyde

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"text/template"

	"github.com/urmzd/saige/rag/types"
)

// Config holds HyDE transformer parameters.
type Config struct {
	LLM             types.LLM
	NumHypothetical int
	PromptTemplate  string
}

// Transformer generates hypothetical answer documents via an LLM to improve retrieval recall.
type Transformer struct {
	cfg  Config
	tmpl *template.Template
}

// New creates a HyDE query transformer. NumHypothetical defaults to 3 if <= 0.
// It panics when PromptTemplate does not parse; use Compile to receive that
// failure as an error.
func New(cfg Config) *Transformer {
	t, err := Compile(cfg)
	if err != nil {
		panic(err)
	}
	return t
}

// Compile creates a HyDE query transformer like New, but returns an error
// instead of panicking when PromptTemplate does not parse.
func Compile(cfg Config) (*Transformer, error) {
	if cfg.NumHypothetical <= 0 {
		cfg.NumHypothetical = 3
	}
	tmpl := defaultPromptTmpl
	if cfg.PromptTemplate != "" {
		parsed, err := template.New("custom").Parse(cfg.PromptTemplate)
		if err != nil {
			return nil, fmt.Errorf("hyde: parse prompt template: %w", err)
		}
		tmpl = parsed
	}
	return &Transformer{cfg: cfg, tmpl: tmpl}, nil
}

// Transform generates hypothetical documents and returns the original query
// followed by every non-empty hypothetical.
//
// Generations run concurrently and independently: one failure does not
// cancel the others. When some generations fail, Transform returns the
// original query and the successful hypotheticals together with an error
// describing the failures, so the pipeline can search with them and report a
// partial failure. When the prompt cannot be rendered, it returns only the
// original query and the error.
func (t *Transformer) Transform(ctx context.Context, query string) ([]string, error) {
	prompt, err := renderPrompt(t.tmpl, map[string]any{"Query": query})
	if err != nil {
		return []string{query}, err
	}

	hypotheticals := make([]string, t.cfg.NumHypothetical)
	errs := make([]error, t.cfg.NumHypothetical)

	var wg sync.WaitGroup
	for i := range t.cfg.NumHypothetical {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := t.cfg.LLM.Generate(ctx, prompt)
			if err != nil {
				errs[i] = fmt.Errorf("generate hypothetical %d: %w", i, err)
				return
			}
			hypotheticals[i] = strings.TrimSpace(h)
		}()
	}
	wg.Wait()

	queries := make([]string, 0, 1+t.cfg.NumHypothetical)
	queries = append(queries, query)
	for _, h := range hypotheticals {
		if h != "" {
			queries = append(queries, h)
		}
	}
	return queries, errors.Join(errs...)
}
