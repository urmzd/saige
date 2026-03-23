// Package hyde implements Hypothetical Document Embeddings (HyDE) query transformation.
package hyde

import (
	"context"
	"fmt"
	"text/template"

	"golang.org/x/sync/errgroup"

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
func New(cfg Config) *Transformer {
	if cfg.NumHypothetical <= 0 {
		cfg.NumHypothetical = 3
	}
	tmpl := defaultPromptTmpl
	if cfg.PromptTemplate != "" {
		tmpl = template.Must(template.New("custom").Parse(cfg.PromptTemplate))
	}
	return &Transformer{cfg: cfg, tmpl: tmpl}
}

// Transform generates hypothetical documents and returns the original query plus all hypotheticals.
func (t *Transformer) Transform(ctx context.Context, query string) ([]string, error) {
	prompt := renderPrompt(t.tmpl, map[string]any{"Query": query})
	hypotheticals := make([]string, t.cfg.NumHypothetical)

	g, gctx := errgroup.WithContext(ctx)
	for i := range t.cfg.NumHypothetical {
		g.Go(func() error {
			h, err := t.cfg.LLM.Generate(gctx, prompt)
			if err != nil {
				return fmt.Errorf("generate hypothetical %d: %w", i, err)
			}
			hypotheticals[i] = h
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	queries := make([]string, 0, 1+t.cfg.NumHypothetical)
	queries = append(queries, query)
	queries = append(queries, hypotheticals...)
	return queries, nil
}
