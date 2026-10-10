package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/registry"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/agent/workspace"
	topeval "github.com/urmzd/saige/eval"
	"github.com/urmzd/saige/internal/must"
)

type weatherIn struct {
	City  string `json:"city" description:"City name"`
	Units string `json:"units,omitempty" enum:"metric,imperial"`
	Days  int    `json:"days,omitempty"`
}

type weatherOut struct {
	TempC float64 `json:"temp_c"`
	City  string  `json:"city"`
}

type weatherDeps struct{ base float64 }

func weatherTool(opts ...FuncOption) types.Tool {
	return Func("weather", "Current weather for a city", func(rc RunContext[*weatherDeps], in weatherIn) (weatherOut, error) {
		return weatherOut{TempC: rc.Deps.base, City: in.City}, nil
	}, opts...)
}

func TestFuncSchemaFromInput(t *testing.T) {
	def := weatherTool(Capability(types.ToolCapabilityRead)).Definition()
	if def.Name != "weather" || def.Capability != types.ToolCapabilityRead {
		t.Fatalf("def = %+v", def)
	}
	if !slices.Equal(def.Parameters.Required, []string{"city"}) {
		t.Fatalf("required = %v", def.Parameters.Required)
	}
	if p := def.Parameters.Properties["city"]; p.Type != types.SchemaString || p.Description != "City name" {
		t.Fatalf("city = %+v", p)
	}
	if p := def.Parameters.Properties["units"]; !slices.Equal(p.Enum, []string{"metric", "imperial"}) {
		t.Fatalf("units = %+v", p)
	}
	if p := def.Parameters.Properties["days"]; p.Type != types.SchemaInteger {
		t.Fatalf("days = %+v", p)
	}
}

func TestFuncPanicsOnNonStructInput(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("no panic for a non-struct input")
		}
	}()
	Func("bad", "", func(RunContext[NoDeps], string) (string, error) { return "", nil })
}

func TestFuncDecodeErrors(t *testing.T) {
	ctx := ContextWithDeps(context.Background(), &weatherDeps{base: 20})
	tool := weatherTool()
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{name: "unknown property", args: map[string]any{"city": "Oslo", "country": "NO"}, want: "unknown field"},
		{name: "wrong type", args: map[string]any{"city": 7}, want: "cannot unmarshal number"},
		{name: "fraction for an integer", args: map[string]any{"city": "Oslo", "days": 1.5}, want: "cannot unmarshal number"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tool.Execute(ctx, tt.args)
			if !errors.Is(err, types.ErrInvalidToolArguments) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want invalid arguments containing %q", err, tt.want)
			}
		})
	}
}

func TestFuncDecodeErrorReachesModel(t *testing.T) {
	p := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("c1", "weather", map[string]any{"city": "Oslo", "zip": "0150"}),
		agenttest.TextResponse("sorry"),
	}}
	a := must.Get(New(Config{Provider: p, Tools: types.NewToolRegistry(weatherTool())}, WithDeps(&weatherDeps{})))
	deltas := agenttest.CollectDeltas(a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("weather?"))}).Deltas())
	end, ok := endDeltaFor(deltas, "c1")
	if !ok || !strings.Contains(end.Error, types.ErrInvalidToolArguments.Error()) || !strings.Contains(end.Error, "zip") {
		t.Fatalf("end = %+v", end)
	}
}

func TestFuncOutputEncoding(t *testing.T) {
	ctx := ContextWithDeps(context.Background(), &weatherDeps{base: 21.5})
	got, err := weatherTool().Execute(ctx, map[string]any{"city": "Oslo"})
	if err != nil || got != `{"temp_c":21.5,"city":"Oslo"}` {
		t.Fatalf("got %q, %v", got, err)
	}
	echo := Func("echo", "", func(_ RunContext[NoDeps], in struct {
		Text string `json:"text"`
	}) (string, error) {
		return in.Text, nil
	})
	if got, err := echo.Execute(context.Background(), map[string]any{"text": `"quoted" as is`}); err != nil || got != `"quoted" as is` {
		t.Fatalf("string output = %q, %v", got, err)
	}
}

func TestFuncDeps(t *testing.T) {
	tool := weatherTool()
	args := map[string]any{"city": "Oslo"}
	if _, err := tool.Execute(context.Background(), args); err == nil || !strings.Contains(err.Error(), "no dependencies") {
		t.Fatalf("missing deps: %v", err)
	}
	if _, err := tool.Execute(ContextWithDeps(context.Background(), "wrong"), args); err == nil || !strings.Contains(err.Error(), "want *agent.weatherDeps") {
		t.Fatalf("wrong deps: %v", err)
	}
	// Run deps win over the agent's.
	ctx := context.WithValue(context.Background(), agentDepsKey{}, &weatherDeps{base: 1})
	got, err := tool.Execute(ContextWithDeps(ctx, &weatherDeps{base: 2}), args)
	if err != nil || !strings.Contains(got, `"temp_c":2`) {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestFuncRunContext(t *testing.T) {
	ws := workspace.NewMemory()
	var seen RunContext[*weatherDeps]
	tool := Func("probe", "", func(rc RunContext[*weatherDeps], _ struct{}) (string, error) {
		seen = rc
		return "ok", nil
	}, Approval("run the probe"))
	p := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("c1", "probe", map[string]any{}),
		agenttest.TextResponse("done"),
	}}
	a := must.Get(New(Config{Name: "host", Provider: p, Tools: types.NewToolRegistry(tool)},
		WithDeps(&weatherDeps{base: 3}), WithWorkspace(ws),
		WithToolContext(types.NewToolContext(map[string]any{types.ToolContextIdempotencyKey: "key-1"}))))
	stream := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("probe"))})
	for d := range stream.Deltas() {
		if m, ok := d.(types.MarkerDelta); ok {
			if err := stream.ResolveMarkerErr(m.ToolCallID, Resolution{Approved: true, Approver: "user:ada"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := stream.Wait(); err != nil {
		t.Fatal(err)
	}
	if seen.Deps == nil || seen.Deps.base != 3 {
		t.Fatalf("deps = %+v", seen.Deps)
	}
	if seen.Call.ID != "c1" || seen.Call.Name != "probe" || seen.Call.Agent != "host" || seen.RunID() == "" || seen.Branch() == "" {
		t.Fatalf("call = %+v", seen.Call)
	}
	if seen.Workspace == nil || seen.IdempotencyKey != "key-1" {
		t.Fatalf("workspace %v, key %q", seen.Workspace, seen.IdempotencyKey)
	}
	if !seen.Approval.Required || seen.Approval.Approver != "user:ada" {
		t.Fatalf("approval = %+v", seen.Approval)
	}
	if seen.Err() != nil {
		t.Fatal("RunContext is not a usable context")
	}
}

func TestFuncOptions(t *testing.T) {
	tool := weatherTool(Approval("check"), Idempotent(), Capability(types.ToolCapabilityWrite))
	mt, ok := tool.(*types.MarkedTool)
	if !ok || mt.Markers[0].Kind != "human_approval" || mt.Markers[0].Message != "check" {
		t.Fatalf("tool = %#v", tool)
	}
	if !types.IsIdempotent(tool) || types.IsIdempotent(weatherTool()) {
		t.Fatal("idempotence not declared through the marker")
	}
	if types.ToolVersion(tool) == "" || types.ToolVersion(tool) != types.DefinitionHash(tool.Definition()) {
		t.Fatal("version not visible through the marker")
	}
}

func TestFuncVersionFollowsSchema(t *testing.T) {
	type v1 struct {
		Path string `json:"path"`
	}
	type v2 struct {
		Path  string `json:"path"`
		Force bool   `json:"force,omitempty"`
	}
	fn1 := func(RunContext[NoDeps], v1) (string, error) { return "", nil }
	fn2 := func(RunContext[NoDeps], v2) (string, error) { return "", nil }
	a := Func("rm", "Remove a file", fn1)
	b := Func("rm", "Remove a file", fn1)
	c := Func("rm", "Remove a file", fn2)
	if types.ToolVersion(a) != types.ToolVersion(b) {
		t.Fatal("same schema, different versions")
	}
	if types.ToolVersion(a) == types.ToolVersion(c) {
		t.Fatal("changed input, same version")
	}
	if types.ToolVersion(a) == types.ToolVersion(Func("rm", "Remove a file", fn1, Capability(types.ToolCapabilityDestructive))) {
		t.Fatal("changed capability, same version")
	}

	set := registry.NewToolSet()
	e1 := set.MustRegister(a)
	e2 := set.MustRegister(b)
	if e1.Revision != 1 || e2.Revision != 1 || len(set.History("rm")) != 1 {
		t.Fatalf("unchanged tool added a revision: %d, %d", e1.Revision, e2.Revision)
	}
	e3 := set.MustRegister(c)
	if e3.Revision != 2 || e3.Version != types.ToolVersion(c) {
		t.Fatalf("changed tool: %+v", e3)
	}
	old, ok := set.AtVersion("rm", types.ToolVersion(a))
	if !ok || old.Revision != 1 {
		t.Fatalf("AtVersion = %+v, %v", old, ok)
	}
	// A tool without a version keeps the old append-always behaviour.
	plain := &agenttest.MockTool{Def: types.ToolDef{Name: "plain"}}
	set.MustRegister(plain)
	if e := set.MustRegister(plain); e.Revision != 2 {
		t.Fatalf("unversioned revision = %d", e.Revision)
	}
}

func TestFuncVersionRecorded(t *testing.T) {
	tool := weatherTool()
	p := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("c1", "weather", map[string]any{"city": "Oslo"}),
		agenttest.TextResponse("warm"),
	}}
	a := must.Get(New(Config{Provider: p, Tools: types.NewToolRegistry(tool)}, WithDeps(&weatherDeps{base: 30})))
	deltas := agenttest.CollectDeltas(a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("weather?"))}).Deltas())
	want := types.ToolVersion(tool)
	if end, _ := endDeltaFor(deltas, "c1"); end.Version != want {
		t.Fatalf("delta version = %q, want %q", end.Version, want)
	}
	results := toolResults(t, a)
	if len(results) != 1 || results[0].ToolVersion != want {
		t.Fatalf("tree results = %+v", results)
	}
	// The version survives serialization of the tree.
	raw, err := json.Marshal(a.Tree())
	if err != nil || !strings.Contains(string(raw), `"tool_version":"`+want+`"`) {
		t.Fatalf("serialized tree lacks the version: %v", err)
	}
	// The wire format carries it too.
	env, err := types.NewDeltaEnvelope(types.ToolExecEndDelta{ToolCallID: "c1", Version: want})
	if err != nil {
		t.Fatal(err)
	}
	back, err := env.Delta()
	if err != nil || back.(types.ToolExecEndDelta).Version != want {
		t.Fatalf("wire round trip = %+v, %v", back, err)
	}

	var prov, other topeval.Provenance
	prov.AddTool("weather", want)
	other.AddTool("weather", "0000")
	if prov.Tools["weather"] != want {
		t.Fatalf("provenance = %+v", prov.Tools)
	}
	if drift := topeval.ConfigDrift(prov, other); len(drift) != 1 || !strings.Contains(drift[0], "tool weather") {
		t.Fatalf("drift = %v", drift)
	}
}
