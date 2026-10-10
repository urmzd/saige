package types

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
)

func ptrF(v float64) *float64 { return &v }
func ptrI(v int64) *int64     { return &v }
func ptrB(v bool) *bool       { return &v }
func ptrS(v string) *string   { return &v }

// spaceFor builds the ParamSpace that states the same rules as mc, the way
// a catalog offering would declare them.
func spaceFor(mc ModelCapabilities) ParamSpace {
	no := ptrB(false)
	s := ParamSpace{Params: map[ParamName]ParamSpec{}}
	declare := func(name ParamName, c Capability, spec ParamSpec) {
		if !mc.Supports(c) {
			spec.Allowed = no
		}
		s.Params[name] = spec
	}
	declare(ParamTemperature, CapTemperature, ParamSpec{Type: ParamTypeNumber, Min: ptrF(0), Max: ptrF(2)})
	declare(ParamTopP, CapTopP, ParamSpec{Type: ParamTypeNumber, Min: ptrF(0), Max: ptrF(1)})
	declare(ParamTopK, CapTopK, ParamSpec{Type: ParamTypeNumber, Min: ptrF(0)})
	declare(ParamFrequencyPenalty, CapFrequencyPenalty, ParamSpec{Type: ParamTypeNumber, Min: ptrF(-2), Max: ptrF(2)})
	declare(ParamPresencePenalty, CapPresencePenalty, ParamSpec{Type: ParamTypeNumber, Min: ptrF(-2), Max: ptrF(2)})
	declare(ParamSeed, CapSeed, ParamSpec{Type: ParamTypeInteger})
	maxOut := ParamSpec{Type: ParamTypeInteger, Min: ptrF(1)}
	if mc.MaxOutputTokens > 0 {
		maxOut.Max = ptrF(float64(mc.MaxOutputTokens))
	}
	declare(ParamMaxOutputTokens, CapMaxOutputTokens, maxOut)
	declare(ParamStop, CapStopSequences, ParamSpec{Type: ParamTypeStringList})
	declare(ParamParallelTools, CapParallelToolControl, ParamSpec{Type: ParamTypeBoolean})
	choices := []string{"none", "required", "named"}
	if mc.RejectsForcedToolChoice {
		choices = []string{"none"}
	}
	declare(ParamToolChoice, CapToolChoice, ParamSpec{Type: ParamTypeEnum, Values: choices})
	declare(ParamReasoningEffort, CapReasoningEffort, ParamSpec{Type: ParamTypeEnum, Values: mc.ReasoningEfforts,
		Required: mc.ReasoningRequired, Default: mc.DefaultReasoningEffort})
	budget := ParamSpec{Type: ParamTypeInteger, Required: mc.ReasoningRequired, Special: map[string]string{}}
	if mc.MinReasoningBudget > 0 {
		budget.Min = ptrF(float64(mc.MinReasoningBudget))
	}
	if mc.MaxReasoningBudget > 0 {
		budget.Max = ptrF(float64(mc.MaxReasoningBudget))
	}
	if mc.DynamicReasoningBudget {
		budget.Special["-1"] = "dynamic"
	}
	if mc.ZeroReasoningBudget {
		budget.Special["0"] = "off"
	}
	declare(ParamReasoningBudget, CapReasoningBudget, budget)
	declare(ParamReasoningEnabled, CapReasoningToggle, ParamSpec{Type: ParamTypeBoolean, Required: mc.ReasoningRequired,
		Default: mc.ReasoningRequired || mc.ReasoningDefaultEnabled})
	s.Constraints = append(s.Constraints, Constraint{
		Exclusive: []ParamName{ParamReasoningEnabled, ParamReasoningBudget, ParamReasoningEffort},
	})
	var forbid []ParamName
	for _, c := range mc.SamplingRequiresNoReasoning {
		forbid = append(forbid, ParamName(c))
	}
	if len(forbid) > 0 {
		for _, when := range []Condition{
			{string(ParamReasoningEnabled): {Bool: ptrB(true)}},
			{string(ParamReasoningBudget): {Set: ptrB(true), Not: []string{"0"}}},
			{string(ParamReasoningEffort): {Set: ptrB(true), Not: []string{"none"}}},
		} {
			s.Constraints = append(s.Constraints, Constraint{When: when, Forbid: forbid})
		}
	}
	return s
}

func TestParamSpaceValidateMatchesValidateOptions(t *testing.T) {
	sampling := ModelCapabilities{Provider: "p", Model: "m", MaxOutputTokens: 1000, Caps: map[Capability]bool{
		CapTemperature: true, CapTopP: true, CapMaxOutputTokens: true, CapSeed: true, CapStopSequences: true,
		CapToolChoice: true, CapFrequencyPenalty: true, CapPresencePenalty: true,
	}}
	reasoning := ModelCapabilities{Provider: "p", Model: "r", Caps: map[Capability]bool{
		CapTemperature: true, CapTopP: true, CapReasoningEffort: true, CapReasoningBudget: true, CapReasoningToggle: true,
		CapToolChoice: true,
	}, ReasoningEfforts: []string{"none", "low", "high"}, MinReasoningBudget: 1024, MaxReasoningBudget: 32000,
		DynamicReasoningBudget: true, ZeroReasoningBudget: true,
		SamplingRequiresNoReasoning: []Capability{CapTemperature, CapTopP}, RejectsForcedToolChoice: true}
	required := reasoning
	required.Model, required.ReasoningRequired = "q", true
	required.ReasoningEfforts = []string{"low", "high"}
	required.ZeroReasoningBudget = false

	opts := []RequestOptions{
		{},
		{Temperature: ptrF(0.5)},
		{Temperature: ptrF(3)},
		{Temperature: ptrF(math.NaN())},
		{TopP: ptrF(1.5)},
		{TopK: ptrF(5)},
		{FrequencyPenalty: ptrF(-3)},
		{Seed: ptrI(7)},
		{MaxOutputTokens: ptrI(0)},
		{MaxOutputTokens: ptrI(500)},
		{MaxOutputTokens: ptrI(5000)},
		{StopSequences: []string{"x"}},
		{ParallelTools: ptrB(true)},
		{ToolChoice: &ToolChoice{Mode: ToolChoiceRequired}},
		{ToolChoice: &ToolChoice{Mode: ToolChoiceNone}},
		{ReasoningEffort: ptrS("low")},
		{ReasoningEffort: ptrS("max")},
		{ReasoningEffort: ptrS("none")},
		{ReasoningEffort: ptrS("none"), Temperature: ptrF(1)},
		{ReasoningEffort: ptrS("high"), Temperature: ptrF(1)},
		{ReasoningBudget: ptrI(2048)},
		{ReasoningBudget: ptrI(100)},
		{ReasoningBudget: ptrI(-1)},
		{ReasoningBudget: ptrI(0)},
		{ReasoningBudget: ptrI(0), TopP: ptrF(0.5)},
		{ReasoningBudget: ptrI(4096), TopP: ptrF(0.5)},
		{ReasoningEnabled: ptrB(false)},
		{ReasoningEnabled: ptrB(true), Temperature: ptrF(1)},
		{ReasoningEnabled: ptrB(true), ReasoningEffort: ptrS("low")},
	}
	for _, mc := range []ModelCapabilities{sampling, reasoning, required} {
		space := spaceFor(mc)
		for i, o := range opts {
			t.Run(fmt.Sprintf("%s/%d", mc.Model, i), func(t *testing.T) {
				want := mc.ValidateOptions(o)
				got := space.Validate(o, RequestShape{Provider: mc.Provider, Model: mc.Model})
				if fmt.Sprint(got) != fmt.Sprint(want) {
					t.Fatalf("Validate(%+v)\n got: %v\nwant: %v", o, got, want)
				}
				if got != nil && !errors.Is(got, ErrInvalidModelConfig) {
					t.Fatalf("error does not match ErrInvalidModelConfig: %v", got)
				}
			})
		}
	}
}

func TestParamSpaceConstraintsOnRequestShape(t *testing.T) {
	space := ParamSpace{
		Params: map[ParamName]ParamSpec{
			ParamReasoningEffort: {Type: ParamTypeEnum, Values: []string{"none", "low"}, Default: "low"},
			ParamTemperature:     {Type: ParamTypeNumber, Min: ptrF(0), Max: ptrF(2)},
		},
		Constraints: []Constraint{
			{When: Condition{CondRequestTools: {Bool: ptrB(true)}},
				Require: map[ParamName][]string{ParamReasoningEffort: {"none"}},
				Reason:  "Chat Completions takes tools only with effort none"},
			{When: Condition{string(ParamReasoningEffort): {Not: []string{"none"}}},
				Forbid: []ParamName{ParamTemperature}, Reason: "sampling requires reasoning effort none"},
		},
	}
	tests := []struct {
		o       RequestOptions
		shape   RequestShape
		wantErr string
	}{
		{RequestOptions{ReasoningEffort: ptrS("low")}, RequestShape{}, ""},
		{RequestOptions{ReasoningEffort: ptrS("low")}, RequestShape{Tools: true}, "takes tools only with effort none"},
		{RequestOptions{ReasoningEffort: ptrS("none")}, RequestShape{Tools: true}, ""},
		// Unset effort is tested at its default, which forbids sampling.
		{RequestOptions{Temperature: ptrF(1)}, RequestShape{}, "sampling requires reasoning effort none"},
		{RequestOptions{Temperature: ptrF(1), ReasoningEffort: ptrS("none")}, RequestShape{}, ""},
	}
	for i, tt := range tests {
		err := space.Validate(tt.o, tt.shape)
		if tt.wantErr == "" && err != nil || tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
			t.Errorf("case %d: err = %v, want %q", i, err, tt.wantErr)
		}
	}
}

func TestParamSpaceIntersect(t *testing.T) {
	a := ParamSpace{Params: map[ParamName]ParamSpec{
		ParamTemperature:     {Type: ParamTypeNumber, Min: ptrF(0), Max: ptrF(2)},
		ParamReasoningEffort: {Type: ParamTypeEnum, Values: []string{"low", "high", "max"}},
		ParamTopK:            {Type: ParamTypeNumber},
	}, Constraints: []Constraint{{Exclusive: []ParamName{ParamReasoningBudget, ParamReasoningEffort}}}}
	b := ParamSpace{Params: map[ParamName]ParamSpec{
		ParamTemperature:     {Type: ParamTypeNumber, Min: ptrF(0.5), Max: ptrF(1)},
		ParamReasoningEffort: {Type: ParamTypeEnum, Values: []string{"high", "low"}, Required: true},
		ParamTopK:            {Allowed: ptrB(false)},
	}}
	got := a.Intersect(b)
	temp := got.Params[ParamTemperature]
	if *temp.Min != 0.5 || *temp.Max != 1 {
		t.Errorf("temperature range = [%v, %v]", *temp.Min, *temp.Max)
	}
	if eff := got.Params[ParamReasoningEffort]; !slices.Equal(eff.Values, []string{"low", "high"}) || !eff.Required {
		t.Errorf("effort = %+v", eff)
	}
	if _, ok := got.Params[ParamTopK]; ok {
		t.Error("a parameter one side forbids survived")
	}
	if len(got.Constraints) != 1 {
		t.Errorf("constraints = %+v", got.Constraints)
	}
	if err := got.Validate(RequestOptions{Temperature: ptrF(1.5)}, RequestShape{}); err == nil {
		t.Error("intersection accepted a value outside the narrower range")
	}
	if err := got.Validate(RequestOptions{TopK: ptrF(1)}, RequestShape{}); err == nil {
		t.Error("intersection accepted a forbidden parameter")
	}
}
