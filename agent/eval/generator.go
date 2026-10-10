package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/urmzd/saige/agent/convert"
	"github.com/urmzd/saige/agent/provider/wrapper"
	"github.com/urmzd/saige/agent/types"
	topeval "github.com/urmzd/saige/eval"
)

// Generator adapts an agent provider to the eval judge interfaces, so an LLM
// judge can run on any provider. It implements [topeval.StructuredGenerator]:
// when the provider supports structured output, a judge's verdict is
// constrained to the judge schema; otherwise the request is sent as plain
// text and the judge parses the reply.
//
// It also implements [topeval.PartsGenerator], so a judge can see the media
// of a case's input. Media goes through the modality conversion policy: a
// part the judge model takes natively is sent as it is, and any other part
// rejects the call unless [WithConversion] permits an action for it, such as
// describe or extract.
type Generator struct {
	provider   types.Provider
	system     string
	conversion types.ConversionPolicy
}

var (
	_ topeval.StructuredGenerator = (*Generator)(nil)
	_ topeval.PartsGenerator      = (*Generator)(nil)
)

// GeneratorOption configures a [Generator].
type GeneratorOption func(*Generator)

// WithSystemPrompt sends a system message before every prompt.
func WithSystemPrompt(text string) GeneratorOption {
	return func(g *Generator) { g.system = text }
}

// WithConversion sets the conversion policy that fits media to the judge
// model, as agent.WithConversion does for an agent: the modality dial, the
// converters its actions use, the cache, and the cost cap. Without it, media
// the judge model cannot take natively fails the score.
func WithConversion(p types.ConversionPolicy) GeneratorOption {
	return func(g *Generator) { g.conversion = p }
}

// NewGenerator returns a [Generator] over p. A provider that does not plan
// conversions itself (one not built by provider.Build) is wrapped in a
// conversion decorator, so media is planned the same way either way.
func NewGenerator(p types.Provider, opts ...GeneratorOption) *Generator {
	g := &Generator{provider: p}
	for _, o := range opts {
		o(g)
	}
	if _, ok := wrapper.As[types.ConversionPlanner](g.provider); !ok {
		if cp, err := convert.New(g.provider, convert.Config{Policy: types.ConversionPolicy{Cache: g.conversion.Cache}}); err == nil {
			g.provider = cp
		}
	}
	return g
}

// stream sends req under the generator's conversion policy.
func (g *Generator) stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	if !g.conversion.IsZero() {
		ctx = convert.WithRuntime(ctx, convert.Runtime{Policy: g.conversion})
	}
	return g.provider.Stream(ctx, req)
}

// GenerateParts sends parts as one user message, with schema as the
// response schema when it is set and the provider supports structured
// output.
func (g *Generator) GenerateParts(ctx context.Context, parts []types.UserPart, schema json.RawMessage) (string, error) {
	msgs := make([]types.Message, 0, 2)
	if g.system != "" {
		msgs = append(msgs, types.SystemMsg(types.Text(g.system)))
	}
	msgs = append(msgs, types.UserMsg(parts...))
	req := types.Request{Messages: msgs}
	if len(schema) > 0 && types.AcceptsSchema(g.provider) {
		var ps types.ParameterSchema
		if err := json.Unmarshal(schema, &ps); err != nil {
			return "", fmt.Errorf("response schema: %w", err)
		}
		req.Schema = &ps
	}
	ch, err := g.stream(ctx, req)
	if err != nil {
		return "", err
	}
	return collectReply(ch)
}

// Generate sends prompt as a single user message and returns the reply text.
func (g *Generator) Generate(ctx context.Context, prompt string) (string, error) {
	ch, err := g.stream(ctx, types.Request{Messages: g.messages(prompt)})
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
	ch, err := g.stream(ctx, types.Request{Messages: g.messages(prompt), Schema: &ps})
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
