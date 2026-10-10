package online

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
}

var _ eval.StructuredGenerator = (*BudgetedGenerator)(nil)

// Generate implements [eval.Generator].
func (g *BudgetedGenerator) Generate(ctx context.Context, prompt string) (string, error) {
	return g.call(ctx, prompt, nil)
}

// GenerateStructured implements [eval.StructuredGenerator]. The schema is
// sent when the provider supports structured output and dropped otherwise;
// the judge parses either reply.
func (g *BudgetedGenerator) GenerateStructured(ctx context.Context, prompt string, schema json.RawMessage) (string, error) {
	if _, ok := g.Provider.(types.StructuredOutputProvider); !ok {
		return g.call(ctx, prompt, nil)
	}
	var ps types.ParameterSchema
	if err := json.Unmarshal(schema, &ps); err != nil {
		return "", fmt.Errorf("response schema: %w", err)
	}
	return g.call(ctx, prompt, &ps)
}

func (g *BudgetedGenerator) call(ctx context.Context, prompt string, schema *types.ParameterSchema) (string, error) {
	caps, _ := types.ProviderCapabilities(g.Provider)
	id := types.NewID()
	if g.Budget != nil {
		if _, err := g.Budget.Reserve(id, caps.Pricing); err != nil {
			return "", fmt.Errorf("%w: %w", types.ErrBudgetAdmission, err)
		}
	}
	msgs := make([]types.Message, 0, 2)
	if g.System != "" {
		msgs = append(msgs, types.NewSystemMessage(g.System))
	}
	msgs = append(msgs, types.NewUserMessage(prompt))

	var (
		ch  <-chan types.Delta
		err error
	)
	if schema != nil {
		ch, err = g.Provider.(types.StructuredOutputProvider).ChatStreamWithSchema(ctx, msgs, nil, schema)
	} else {
		ch, err = g.Provider.ChatStream(ctx, msgs, nil)
	}
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
		case types.TextContentDelta:
			text.WriteString(v.Content)
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
