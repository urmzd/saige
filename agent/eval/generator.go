package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/urmzd/saige/agent/types"
	topeval "github.com/urmzd/saige/eval"
)

// Generator adapts an agent provider to the eval judge interfaces, so an LLM
// judge can run on any provider. It implements [topeval.StructuredGenerator]:
// when the provider supports structured output, a judge's verdict is
// constrained to the judge schema; otherwise the request is sent as plain
// text and the judge parses the reply.
type Generator struct {
	provider types.Provider
	system   string
}

var _ topeval.StructuredGenerator = (*Generator)(nil)

// GeneratorOption configures a [Generator].
type GeneratorOption func(*Generator)

// WithSystemPrompt sends a system message before every prompt.
func WithSystemPrompt(text string) GeneratorOption {
	return func(g *Generator) { g.system = text }
}

// NewGenerator returns a [Generator] over p.
func NewGenerator(p types.Provider, opts ...GeneratorOption) *Generator {
	g := &Generator{provider: p}
	for _, o := range opts {
		o(g)
	}
	return g
}

// Generate sends prompt as a single user message and returns the reply text.
func (g *Generator) Generate(ctx context.Context, prompt string) (string, error) {
	ch, err := g.provider.Stream(ctx, types.Request{Messages: g.messages(prompt)})
	if err != nil {
		return "", err
	}
	return collectReply(ch)
}

// GenerateStructured sends prompt with schema as the response schema when
// the provider implements [types.StructuredOutputProvider], and as a plain
// request otherwise. A schema the provider rejects is returned as an error
// rather than silently dropped.
func (g *Generator) GenerateStructured(ctx context.Context, prompt string, schema json.RawMessage) (string, error) {
	if !types.AcceptsSchema(g.provider) {
		return g.Generate(ctx, prompt)
	}
	var ps types.ParameterSchema
	if err := json.Unmarshal(schema, &ps); err != nil {
		return "", fmt.Errorf("response schema: %w", err)
	}
	ch, err := g.provider.Stream(ctx, types.Request{Messages: g.messages(prompt), Schema: &ps})
	if err != nil {
		return "", err
	}
	return collectReply(ch)
}

func (g *Generator) messages(prompt string) []types.Message {
	msgs := make([]types.Message, 0, 2)
	if g.system != "" {
		msgs = append(msgs, types.SystemMsg(types.Text(g.system)))
	}
	return append(msgs, types.UserMsg(types.Text(prompt)))
}

// collectReply drains a provider stream into its text, returning the first
// ErrorDelta as an error.
func collectReply(ch <-chan types.Delta) (string, error) {
	var (
		text    strings.Builder
		errs    []error
		gotText bool
	)
	for d := range ch {
		switch v := d.(type) {
		case types.PartDelta:
			if v.Text != "" {
				text.WriteString(v.Text)
				gotText = true
			}
		case types.ErrorDelta:
			if v.Error != nil {
				errs = append(errs, v.Error)
			} else {
				errs = append(errs, errors.New("provider stream error"))
			}
		}
	}
	if len(errs) > 0 {
		return text.String(), errors.Join(errs...)
	}
	if !gotText {
		return "", errors.New("provider returned no text")
	}
	return text.String(), nil
}
