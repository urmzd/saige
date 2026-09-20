package types_test

import (
	"errors"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestReasoningConstraintsSurviveCopyAndFallback(t *testing.T) {
	a := types.ModelCapabilities{Provider: "test", Model: "a", Known: true,
		Caps:               map[types.Capability]bool{types.CapReasoningBudget: true, types.CapTemperature: true},
		MinReasoningBudget: 1, MaxReasoningBudget: 200, ZeroReasoningBudget: true, DynamicReasoningBudget: true,
		SamplingRequiresNoReasoning: []types.Capability{types.CapTemperature}}
	b := a.ForModel("b")
	b.ReasoningRequired = true
	b.MinReasoningBudget, b.MaxReasoningBudget = 100, 150
	b.ZeroReasoningBudget, b.DynamicReasoningBudget = false, false
	b.SamplingRequiresNoReasoning[0] = types.CapTopP
	if a.SamplingRequiresNoReasoning[0] != types.CapTemperature {
		t.Fatal("copy aliases constraints")
	}
	shared := a.Intersect(b)
	for _, n := range []int64{0, -1, 99, 151} {
		if err := shared.ValidateOptions(types.RequestOptions{ReasoningBudget: &n}); !errors.Is(err, types.ErrInvalidModelConfig) || types.IsTransient(err) {
			t.Fatalf("budget %d: %v", n, err)
		}
	}
	n := int64(100)
	if err := shared.ValidateOptions(types.RequestOptions{ReasoningBudget: &n}); err != nil {
		t.Fatal(err)
	}
	zero := 0.0
	if err := shared.ValidateOptions(types.RequestOptions{Temperature: &zero}); err == nil {
		t.Fatal("fallback dropped required-reasoning sampling constraint")
	}
}

func TestExplicitFalseAndUnknownSupport(t *testing.T) {
	f := false
	unknown := types.ModelCapabilities{Provider: "test", Model: "unknown"}
	if err := unknown.ValidateOptions(types.RequestOptions{ParallelTools: &f}); !errors.Is(err, types.ErrInvalidModelConfig) {
		t.Fatalf("explicit false dropped: %v", err)
	}
	if err := unknown.ValidateOptions(types.RequestOptions{}); err != nil {
		t.Fatal(err)
	}
	required := unknown.With(types.CapReasoningToggle)
	required.ReasoningRequired = true
	if err := required.ValidateOptions(types.RequestOptions{ReasoningEnabled: &f}); err == nil {
		t.Fatal("required reasoning disabled")
	}
}
