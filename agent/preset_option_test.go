package agent

import (
	"testing"
	"time"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
)

type stubPreset struct {
	p types.Provider
	d types.PresetDefaults
}

func (s stubPreset) Provider() types.Provider       { return s.p }
func (s stubPreset) Defaults() types.PresetDefaults { return s.d }

func TestWithPresetDefaults(t *testing.T) {
	p := &agenttest.ScriptedProvider{}
	pre := stubPreset{p: p, d: types.PresetDefaults{Name: "x", OutputMode: "tool", LLMTimeout: time.Minute,
		ToolChoice: &types.ToolChoice{Mode: types.ToolChoiceNone}}}
	a := NewAgent(AgentConfig{}, WithPreset(pre))
	if a.cfg.Provider != p || a.cfg.OutputMode != OutputTool || a.cfg.LLMTimeout != time.Minute || a.cfg.ToolChoice.Mode != types.ToolChoiceNone {
		t.Fatalf("defaults not applied: %+v", a.cfg)
	}
	explicit := NewAgent(AgentConfig{LLMTimeout: time.Second, OutputMode: OutputPrompt}, WithPreset(pre), WithLLMTimeout(2*time.Second))
	if explicit.cfg.OutputMode != OutputPrompt || explicit.cfg.LLMTimeout != 2*time.Second {
		t.Fatalf("explicit settings lost: %+v", explicit.cfg)
	}
	auto := NewAgent(AgentConfig{}, WithPreset(stubPreset{p: p, d: types.PresetDefaults{OutputMode: "auto"}}))
	if auto.cfg.OutputMode != OutputAuto {
		t.Fatalf("auto mode: %q", auto.cfg.OutputMode)
	}
}
