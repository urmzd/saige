package types_test

import (
	"errors"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestToolChoiceValidation(t *testing.T) {
	tools := []types.ToolDef{{Name: "search"}, {Name: "fetch"}}
	with := types.ModelCapabilities{Provider: "test", Model: "m", Caps: map[types.Capability]bool{types.CapToolChoice: true}}
	without := types.ModelCapabilities{Provider: "test", Model: "m"}
	tests := []struct {
		name    string
		caps    types.ModelCapabilities
		choice  *types.ToolChoice
		tools   []types.ToolDef
		wantErr bool
	}{
		{"nil is omitted", without, nil, tools, false},
		{"zero mode is auto", without, &types.ToolChoice{}, tools, false},
		{"explicit auto needs no capability", without, &types.ToolChoice{Mode: types.ToolChoiceAuto}, tools, false},
		{"none needs capability", without, &types.ToolChoice{Mode: types.ToolChoiceNone}, tools, true},
		{"none supported", with, &types.ToolChoice{Mode: types.ToolChoiceNone}, tools, false},
		{"required supported", with, &types.ToolChoice{Mode: types.ToolChoiceRequired}, tools, false},
		{"required without tools", with, &types.ToolChoice{Mode: types.ToolChoiceRequired}, []types.ToolDef{}, true},
		{"named supported", with, &types.ToolChoice{Mode: types.ToolChoiceNamed, Name: "fetch"}, tools, false},
		{"named unknown tool", with, &types.ToolChoice{Mode: types.ToolChoiceNamed, Name: "delete"}, tools, true},
		{"named without name", with, &types.ToolChoice{Mode: types.ToolChoiceNamed}, tools, true},
		{"name on auto", with, &types.ToolChoice{Mode: types.ToolChoiceAuto, Name: "fetch"}, tools, true},
		{"name on required", with, &types.ToolChoice{Mode: types.ToolChoiceRequired, Name: "fetch"}, tools, true},
		{"unknown mode", with, &types.ToolChoice{Mode: "any"}, tools, true},
		{"nil tools skips list checks", with, &types.ToolChoice{Mode: types.ToolChoiceNamed, Name: "x"}, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.caps.ValidateToolChoice(tt.choice, tt.tools)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && (!errors.Is(err, types.ErrInvalidModelConfig) || types.IsTransient(err)) {
				t.Errorf("error is not a permanent config error: %v", err)
			}
		})
	}
}

func TestValidateOptionsChecksToolChoice(t *testing.T) {
	without := types.ModelCapabilities{Provider: "test", Model: "m"}
	err := without.ValidateOptions(types.RequestOptions{ToolChoice: &types.ToolChoice{Mode: types.ToolChoiceRequired}})
	if !errors.Is(err, types.ErrInvalidModelConfig) {
		t.Fatalf("unsupported tool_choice accepted: %v", err)
	}
	with := without.With(types.CapToolChoice)
	if err := with.ValidateOptions(types.RequestOptions{ToolChoice: &types.ToolChoice{Mode: types.ToolChoiceNamed, Name: "x"}}); err != nil {
		t.Fatal(err)
	}
}
