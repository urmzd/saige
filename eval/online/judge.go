package online

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/urmzd/saige/agent/convert"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/eval"
)

// BudgetedGenerator is an [eval.StructuredGenerator] whose every provider
// call is admitted by, and charged to, a types.Budget, so LLM judges on
// production traffic cannot spend past a ceiling. Each call reserves
// capacity before it is sent and settles with the usage the provider
// reports; a call that reports no usage is charged its whole reservation, as
// the agent loop charges one.
//
// Use a cheap model: a judge on sampled production runs is called once per
// sampled run and judge.
type BudgetedGenerator struct {
	Provider types.Provider
	// Budget is charged for every call. Nil charges nothing.
	Budget *types.Budget
	// System, when set, is sent as a system message before the prompt.
	System string
	// Conversion fits the media of a run's input to the judge model, as
	// agent.WithConversion does for an agent. The zero value rejects media
	// the model cannot take natively, which fails that judge score. It
	// applies when Provider plans conversions, as every provider built by
	// provider.Build does.
	Conversion types.ConversionPolicy
}

var (
	_ eval.StructuredGenerator = (*BudgetedGenerator)(nil)
	_ eval.PartsGenerator      = (*BudgetedGenerator)(nil)
)

// Generate implements [eval.Generator].
func (g *BudgetedGenerator) Generate(ctx context.Context, prompt string) (string, error) {
	return g.call(ctx, []types.UserPart{types.Text(prompt)}, nil)
}

// GenerateParts implements [eval.PartsGenerator]: parts are sent as one
// user message, under the Conversion policy.
func (g *BudgetedGenerator) GenerateParts(ctx context.Context, parts []types.UserPart, schema json.RawMessage) (string, error) {
	if len(schema) == 0 || !types.AcceptsSchema(g.Provider) {
		return g.call(ctx, parts, nil)
	}
	var ps types.ParameterSchema
	if err := json.Unmarshal(schema, &ps); err != nil {
		return "", fmt.Errorf("response schema: %w", err)
	}
	return g.call(ctx, parts, &ps)
}

// GenerateStructured implements [eval.StructuredGenerator]. The schema is
// sent when the provider supports structured output and dropped otherwise;
// the judge parses either reply.
func (g *BudgetedGenerator) GenerateStructured(ctx context.Context, prompt string, schema json.RawMessage) (string, error) {
	return g.GenerateParts(ctx, []types.UserPart{types.Text(prompt)}, schema)
}

func (g *BudgetedGenerator) call(ctx context.Context, parts []types.UserPart, schema *types.ParameterSchema) (string, error) {
	caps, _ := types.ProviderCapabilities(g.Provider)
	id := types.NewID()
	if g.Budget != nil {
		if _, err := g.Budget.Reserve(id, caps.Pricing); err != nil {
			return "", fmt.Errorf("%w: %w", types.ErrBudgetAdmission, err)
		}
	}
	msgs := make([]types.Message, 0, 2)
	if g.System != "" {
		msgs = append(msgs, types.SystemMsg(types.Text(g.System)))
	}
	msgs = append(msgs, types.UserMsg(parts...))

	if !g.Conversion.IsZero() {
		ctx = convert.WithRuntime(ctx, convert.Runtime{Policy: g.Conversion, Budget: g.Budget, Reservation: id})
	}
	ch, err := g.Provider.Stream(ctx, types.Request{Messages: msgs, Schema: schema})
	if err != nil {
		_ = g.settle(id, caps.Pricing, types.UsageDelta{}, false)
		return "", err
	}
	var (
		text     strings.Builder
		errs     []error
		usage    types.UsageDelta
		gotUsage bool
	)
	for d := range ch {
		switch v := d.(type) {
		case types.PartDelta:
			text.WriteString(v.Text)
		case types.UsageDelta:
			usage, gotUsage = usage.Merge(v), true
		case types.ErrorDelta:
			if v.Error != nil {
				errs = append(errs, v.Error)
			} else {
				errs = append(errs, errors.New("provider stream error"))
			}
		}
	}
	if err := g.settle(id, caps.Pricing, usage, gotUsage); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return text.String(), errors.Join(errs...)
	}
	if text.Len() == 0 {
		return "", errors.New("provider returned no text")
	}
	return text.String(), nil
}

func (g *BudgetedGenerator) settle(id string, pricing types.Pricing, usage types.UsageDelta, known bool) error {
	if g.Budget == nil {
		return nil
	}
	model := usage.ResponseModel
	if model == "" {
		model = types.ProviderModel(g.Provider)
	}
	return g.Budget.Settle(id, model, pricing, types.UsageFromDelta(usage), !known)
}

// overBudget reports whether err is a budget refusal rather than a judge
// failure.
func overBudget(err error) bool {
	return errors.Is(err, types.ErrBudgetAdmission) || errors.Is(err, types.ErrBudgetExceeded) ||
		errors.Is(err, types.ErrBudgetBusy) || errors.Is(err, types.ErrUnpriced)
}
