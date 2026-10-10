package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

type ticket struct {
	Body string `json:"body"`
}

type triage struct {
	Priority string `json:"priority" enum:"low,high"`
}

// keyedPreset is a types.Preset that reports a configuration key, as a
// preset.Bundle does.
type keyedPreset struct {
	p   types.Provider
	key string
}

func (k keyedPreset) Provider() types.Provider       { return k.p }
func (k keyedPreset) Defaults() types.PresetDefaults { return types.PresetDefaults{Name: "triage"} }
func (k keyedPreset) ConfigKey(name string) string   { return k.key + "/" + name }

func TestAIFuncCall(t *testing.T) {
	p := &schemaProvider{schema: [][]types.Delta{agenttest.TextResponse(`{"priority":"high"}`)}}
	f, err := AIFunc[ticket, triage]("triage", "Triage a ticket", AIConfig{
		Prompt: "Triage: {{.Body}}", System: "You triage.", Provider: p,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.Call(context.Background(), ticket{Body: "site down"})
	if err != nil || got.Priority != "high" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if !strings.Contains(lastRequestText(p.lastMsgs), "Triage: site down") {
		t.Fatalf("prompt not rendered: %q", lastRequestText(p.lastMsgs))
	}
}

func TestAIFuncVersion(t *testing.T) {
	p := &agenttest.ScriptedProvider{}
	build := func(cfg AIConfig) string {
		t.Helper()
		f, err := AIFunc[ticket, triage]("triage", "", cfg)
		if err != nil {
			t.Fatal(err)
		}
		return f.Version()
	}
	base := build(AIConfig{Prompt: "Triage: {{.Body}}", Provider: p})
	if base != build(AIConfig{Prompt: "Triage: {{.Body}}", Provider: p}) {
		t.Fatal("same configuration, different versions")
	}
	if base == build(AIConfig{Prompt: "Classify: {{.Body}}", Provider: p}) {
		t.Fatal("prompt change kept the version")
	}
	if base == build(AIConfig{Prompt: "Triage: {{.Body}}", Provider: p, ConfigHash: "abc"}) {
		t.Fatal("config hash change kept the version")
	}
	a := build(AIConfig{Prompt: "Triage: {{.Body}}", Preset: keyedPreset{p: p, key: "k1"}})
	b := build(AIConfig{Prompt: "Triage: {{.Body}}", Preset: keyedPreset{p: p, key: "k2"}})
	if a == b {
		t.Fatal("preset configuration key not in the version")
	}
	f, _ := AIFunc[ticket, struct {
		Priority string `json:"priority"`
		Reason   string `json:"reason"`
	}]("triage", "", AIConfig{Prompt: "Triage: {{.Body}}", Provider: p})
	if f.Version() == base {
		t.Fatal("output schema change kept the version")
	}
}

func TestAIFuncConfigErrors(t *testing.T) {
	p := &agenttest.ScriptedProvider{}
	for name, cfg := range map[string]AIConfig{
		"no prompt":    {Provider: p},
		"no provider":  {Prompt: "x"},
		"both":         {Prompt: "x", Provider: p, Preset: keyedPreset{p: p}},
		"bad template": {Prompt: "{{.Body", Provider: p},
	} {
		if _, err := AIFunc[ticket, triage]("triage", "", cfg); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := AIFunc[ticket, string]("triage", "", AIConfig{Prompt: "x", Provider: p}); err == nil {
		t.Error("non-struct output accepted")
	}
	f, _ := AIFunc[ticket, triage]("triage", "", AIConfig{Prompt: "{{.Missing}}", Provider: p})
	if _, err := f.Call(context.Background(), ticket{}); err == nil {
		t.Error("missing template field rendered")
	}
}

func TestAIFuncTool(t *testing.T) {
	inner := &schemaProvider{schema: [][]types.Delta{agenttest.TextResponse(`{"priority":"low"}`)}}
	f, err := AIFunc[ticket, triage]("triage", "Triage a ticket", AIConfig{Prompt: "Triage: {{.Body}}", Provider: inner})
	if err != nil {
		t.Fatal(err)
	}
	tool := f.Tool()
	if tool.Definition().Capability != types.ToolCapabilityRead || types.ToolVersion(tool) != f.Version() {
		t.Fatalf("tool def %+v, version %q", tool.Definition(), types.ToolVersion(tool))
	}
	outer := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("c1", "triage", map[string]any{"body": "typo"}),
		agenttest.TextResponse("low"),
	}}
	a := must.Get(New(Config{Provider: outer, Tools: types.NewToolRegistry(tool)}))
	if err := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("triage"))}).Wait(); err != nil {
		t.Fatal(err)
	}
	results := toolResults(t, a)
	if len(results) != 1 || results[0].Text() != `{"priority":"low"}` || results[0].ToolVersion != f.Version() {
		t.Fatalf("results = %+v", results)
	}
}
