package types_test

import (
	"errors"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

// limitedModel takes an effort enum and an output cap, and nothing else a
// dial compiles to.
func limitedModel() types.ModelCapabilities {
	mc := capsOf("x", types.CapReasoning, types.CapReasoningEffort, types.CapMaxOutputTokens)
	mc.ReasoningEfforts = []string{"low", "high"}
	mc.MaxOutputTokens = 100
	return mc
}

// TestDialPolicyMatrix pins what each policy does to each dial the model
// cannot honor exactly. "error" means the call is rejected.
func TestDialPolicyMatrix(t *testing.T) {
	focused, seed, off, on := types.CreativityFocused, int64(7), false, true
	big := int64(500)
	dials := map[types.DialName]types.Dials{
		types.DialCreativity:   {Creativity: &focused},
		types.DialReasoning:    {Reasoning: &types.ReasoningDial{Depth: types.DepthMedium}},
		types.DialMaxOutput:    {MaxOutput: &big},
		types.DialReproducible: {Seed: &seed},
		types.DialTools:        {Tools: &types.ToolChoice{Mode: types.ToolChoiceRequired}},
	}
	per := func(n types.DialName, h types.Handling) types.DialPolicy {
		return types.DialPolicy{Per: map[types.DialName]types.Handling{n: h}}
	}
	const reject = "error"
	for _, tc := range []struct {
		dial   types.DialName
		policy types.DialPolicy
		want   types.DialAction
	}{
		{types.DialCreativity, types.DialPolicy{}, types.DialDropped},
		{types.DialCreativity, types.StrictDials, reject},
		{types.DialCreativity, per(types.DialCreativity, types.HandlingReject), reject},
		{types.DialCreativity, per(types.DialCreativity, types.HandlingDrop), types.DialDropped},
		{types.DialReasoning, types.DialPolicy{}, types.DialMapped},
		{types.DialReasoning, types.StrictDials, reject},
		{types.DialReasoning, per(types.DialReasoning, types.HandlingDrop), types.DialDropped},
		{types.DialReasoning, per(types.DialReasoning, types.HandlingReject), reject},
		{types.DialMaxOutput, types.DialPolicy{}, types.DialMapped},
		{types.DialMaxOutput, types.StrictDials, reject},
		{types.DialMaxOutput, per(types.DialMaxOutput, types.HandlingDrop), types.DialDropped},
		{types.DialReproducible, types.DialPolicy{}, reject},
		{types.DialReproducible, per(types.DialReproducible, types.HandlingDrop), types.DialDropped},
		{types.DialReproducible, per(types.DialReproducible, types.HandlingNearest), types.DialDropped},
		{types.DialTools, types.DialPolicy{}, reject},
		{types.DialTools, per(types.DialTools, types.HandlingDrop), types.DialDropped},
		// Strict outranks a per-dial loosening.
		{types.DialReproducible, types.DialPolicy{Strict: true, Per: map[types.DialName]types.Handling{types.DialReproducible: types.HandlingDrop}}, reject},
	} {
		t.Run(string(tc.dial)+"/"+tc.policy.String(), func(t *testing.T) {
			_, rep, err := types.ResolveDials(limitedModel(), types.RequestOptions{}, types.DialContext{}, tc.policy,
				types.DialLayer{Scope: types.DialScopeAgent, Dials: dials[tc.dial]})
			if tc.want == reject {
				if !errors.Is(err, types.ErrInvalidModelConfig) {
					t.Fatalf("err = %v, want a rejection", err)
				}
				if d, _ := rep.Decision(tc.dial); d.Action != types.DialRejected {
					t.Fatalf("rejection not recorded: %+v", rep.Decisions)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if d, _ := rep.Decision(tc.dial); d.Action != tc.want {
				t.Fatalf("got %+v, want %s", d, tc.want)
			}
		})
	}
	// Parallel is contractual only when false.
	pc := capsOf("x")
	_, _, err := types.ResolveDials(pc, types.RequestOptions{}, types.DialContext{}, types.DialPolicy{}, types.DialLayer{Dials: types.Dials{Parallel: &off}})
	if !errors.Is(err, types.ErrInvalidModelConfig) {
		t.Fatalf("parallel false: %v", err)
	}
	_, rep, err := types.ResolveDials(pc, types.RequestOptions{}, types.DialContext{}, types.DialPolicy{}, types.DialLayer{Dials: types.Dials{Parallel: &on}})
	if d, _ := rep.Decision(types.DialParallel); err != nil || d.Action != types.DialDropped {
		t.Fatalf("parallel true: %v %+v", err, d)
	}
}

// A contractual dial is loosened only by naming it: loosening another dial,
// or every advisory one, leaves it rejected.
func TestContractualDialNotLoosenedImplicitly(t *testing.T) {
	seed := int64(7)
	for _, pol := range []types.DialPolicy{
		{},
		{Per: map[types.DialName]types.Handling{types.DialCreativity: types.HandlingDrop, types.DialReasoning: types.HandlingDrop,
			types.DialMaxOutput: types.HandlingDrop, types.DialCache: types.HandlingDrop}},
		{Per: map[types.DialName]types.Handling{types.DialTools: types.HandlingDrop}},
	} {
		_, _, err := types.ResolveDials(limitedModel(), types.RequestOptions{}, types.DialContext{}, pol, types.DialLayer{Dials: types.Dials{Seed: &seed}})
		if !errors.Is(err, types.ErrInvalidModelConfig) {
			t.Errorf("policy %s loosened the seed: %v", pol.String(), err)
		}
	}
}

func TestDialPolicyValidateAndString(t *testing.T) {
	bad := []types.DialPolicy{
		{Per: map[types.DialName]types.Handling{"temperature": types.HandlingDrop}},
		{Per: map[types.DialName]types.Handling{types.DialCreativity: "ignore"}},
	}
	for _, p := range bad {
		if err := p.Validate(); !errors.Is(err, types.ErrInvalidModelConfig) {
			t.Errorf("%+v: %v", p, err)
		}
	}
	p := types.DialPolicy{Per: map[types.DialName]types.Handling{types.DialReproducible: types.HandlingDrop, types.DialCreativity: types.HandlingReject}}
	if got := p.String(); got != "default creativity=reject reproducible=drop" {
		t.Fatalf("String = %q", got)
	}
	if types.StrictDials.String() != "strict" || (types.DialPolicy{}).String() != "default" {
		t.Fatal("named policies")
	}
}
