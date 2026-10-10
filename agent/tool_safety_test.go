package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

// panicTool panics on every call.
type panicTool struct{ name string }

func (t panicTool) Definition() types.ToolDef { return types.ToolDef{Name: t.name} }
func (t panicTool) Execute(context.Context, map[string]any) (string, error) {
	panic("tool exploded")
}

// panicHandoffTool is a control-transfer tool whose Execute panics.
type panicHandoffTool struct{}

func (panicHandoffTool) Definition() types.ToolDef { return types.ToolDef{Name: "handoff_to_x"} }
func (panicHandoffTool) HandoffTarget() string     { return "x" }
func (panicHandoffTool) Execute(context.Context, map[string]any) (string, error) {
	panic("handoff exploded")
}

// toolResults returns every tool result on the agent's active branch.
func toolResults(t *testing.T, a *Agent) []types.ToolResultPart {
	t.Helper()
	msgs, err := a.Tree().FlattenBranch(a.Tree().Active())
	if err != nil {
		t.Fatal(err)
	}
	var out []types.ToolResultPart
	for _, m := range msgs {
		var contents []types.SystemPart
		switch v := m.(type) {
		case types.SystemMessage:
			contents = v.Parts
		case types.UserMessage:
			for _, c := range v.Parts {
				if tr, ok := c.(types.ToolResultPart); ok {
					out = append(out, tr)
				}
			}
		}
		for _, c := range contents {
			if tr, ok := c.(types.ToolResultPart); ok {
				out = append(out, tr)
			}
		}
	}
	return out
}

func endDeltaFor(deltas []types.Delta, id string) (types.ToolExecEndDelta, bool) {
	for _, d := range deltas {
		if end, ok := d.(types.ToolExecEndDelta); ok && end.ToolCallID == id {
			return end, true
		}
	}
	return types.ToolExecEndDelta{}, false
}

func TestToolPanicBecomesToolError(t *testing.T) {
	tests := []struct {
		name     string
		tool     types.Tool
		gate     types.ToolGate
		parallel int
	}{
		{name: "tool panic, parallel", tool: panicTool{name: "boom"}, parallel: 0},
		{name: "tool panic, sequential", tool: panicTool{name: "boom"}, parallel: 1},
		{name: "gate panic", tool: &agenttest.MockTool{Def: types.ToolDef{Name: "boom"}}, parallel: 0,
			gate: types.GateFunc(func(context.Context, types.ToolDef, map[string]any) types.GateDecision {
				panic("gate exploded")
			})},
		{name: "handoff tool panic", tool: panicHandoffTool{}, parallel: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name := tt.tool.Definition().Name
			provider := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
				agenttest.ToolCallResponse("c1", name, nil),
				agenttest.TextResponse("recovered"),
			}}
			a := must.Get(New(Config{
				Provider:         provider,
				Tools:            types.NewToolRegistry(tt.tool),
				ToolGate:         tt.gate,
				MaxParallelTools: tt.parallel,
			}))
			stream := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("go"))})
			deltas := agenttest.CollectDeltas(stream.Deltas())
			if err := stream.Wait(); err != nil {
				t.Fatalf("run failed: %v", err)
			}
			end, ok := endDeltaFor(deltas, "c1")
			if !ok || !strings.Contains(end.Error, "panic") {
				t.Fatalf("ToolExecEndDelta = %+v, want an error naming the panic", end)
			}
			results := toolResults(t, a)
			if len(results) != 1 || !results[0].IsError || !strings.Contains(results[0].Text(), "panic") {
				t.Fatalf("tool results = %+v, want one error result naming the panic", results)
			}
		})
	}
}

// Under a durable runner a panic fails the step, so the runner never records
// it as a completed result, and the run stops. The tool_use is still answered.
func TestToolPanicUnderDurableRunnerStopsRun(t *testing.T) {
	runner := newRecordingRunner()
	provider := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("c1", "boom", nil),
		agenttest.TextResponse("unreachable"),
	}}
	a := must.Get(New(Config{Provider: provider, Tools: types.NewToolRegistry(panicTool{name: "boom"}), StepRunner: runner}))
	stream := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("go"))})
	for range stream.Deltas() {
	}
	err := stream.Wait()
	var panicked *toolPanicError
	if !errors.As(err, &panicked) {
		t.Fatalf("Wait = %v, want a tool panic error", err)
	}
	if runner.has("tool-c1") {
		t.Error("a panicking step must not be recorded as completed")
	}
	if results := toolResults(t, a); len(results) != 1 || !results[0].IsError {
		t.Fatalf("tool results = %+v, want the tool_use answered with an error", results)
	}
}

// countingGate counts checks so a test can prove a call never reached it.
type countingGate struct{ calls atomic.Int32 }

func (g *countingGate) Check(context.Context, types.ToolDef, map[string]any) types.GateDecision {
	g.calls.Add(1)
	return types.Allow()
}

func TestInvalidToolArgumentsNeverRunTheTool(t *testing.T) {
	def := types.ToolDef{Name: "write", Parameters: types.ParameterSchema{
		Type: "object", Required: []string{"path"},
		Properties: map[string]types.PropertyDef{"path": {Type: "string"}},
	}}
	tests := []struct {
		name    string
		call    []types.Delta
		wantMsg string
	}{
		{
			name: "malformed JSON reported by the producer",
			call: []types.Delta{
				types.PartStart{Index: 0, Kind: types.KindToolCall, ID: "c1", Name: "write"},
				types.PartEnd{Index: 0, Part: types.ToolCallPart{ID: "c1", Name: "write", ArgumentsError: "unexpected end of JSON input"}},
			},
			wantMsg: "invalid tool arguments: unexpected end of JSON input",
		},
		{
			name: "malformed JSON in the argument stream",
			call: []types.Delta{
				types.PartStart{Index: 1, Kind: types.KindToolCall, ID: "c1", Name: "write"},
				types.PartDelta{Index: 1, Args: `{"path": "/tmp`},
				types.PartEnd{Index: 1},
			},
			wantMsg: "invalid tool arguments",
		},
		{
			name:    "missing required property",
			call:    agenttest.ToolCallResponse("c1", "write", map[string]any{"other": 1}),
			wantMsg: `missing required property "path"`,
		},
		{
			name:    "wrong property type",
			call:    agenttest.ToolCallResponse("c1", "write", map[string]any{"path": 7}),
			wantMsg: `property "path" must be string`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool := &agenttest.MockTool{Def: def, Result: "written"}
			gate := &countingGate{}
			provider := &agenttest.ScriptedProvider{Responses: [][]types.Delta{tt.call, agenttest.TextResponse("ok")}}
			a := must.Get(New(Config{Provider: provider, Tools: types.NewToolRegistry(tool), ToolGate: gate}))
			stream := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("go"))})
			for range stream.Deltas() {
			}
			if err := stream.Wait(); err != nil {
				t.Fatalf("run failed: %v", err)
			}
			if tool.CallCount() != 0 {
				t.Error("the tool ran with invalid arguments")
			}
			if gate.calls.Load() != 0 {
				t.Error("the gate saw invalid arguments")
			}
			results := toolResults(t, a)
			if len(results) != 1 || !results[0].IsError || !strings.Contains(results[0].Text(), tt.wantMsg) {
				t.Fatalf("tool results = %+v, want an error containing %q", results, tt.wantMsg)
			}
		})
	}
}

// Arguments a human edits while approving are gated again, so an approval
// cannot turn a call into one that policy denies.
func TestApprovalEditsAreGatedAgain(t *testing.T) {
	denyEtc := types.GateFunc(func(_ context.Context, _ types.ToolDef, args map[string]any) types.GateDecision {
		if p, _ := args["path"].(string); strings.HasPrefix(p, "/etc") {
			return types.Deny("system paths are off limits")
		}
		return types.Allow()
	})
	tests := []struct {
		name     string
		marked   bool
		edit     map[string]any
		wantRun  bool
		wantText string
	}{
		{name: "gate approval, safe edit", edit: map[string]any{"path": "/tmp/b"}, wantRun: true},
		{name: "gate approval, denied edit", edit: map[string]any{"path": "/etc/passwd"}, wantText: "system paths are off limits"},
		{name: "gate approval, invalid edit", edit: map[string]any{"path": 3}, wantText: "invalid tool arguments"},
		{name: "marker approval, denied edit", marked: true, edit: map[string]any{"path": "/etc/passwd"}, wantText: "system paths are off limits"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			def := types.ToolDef{Name: "write_file", Parameters: types.ParameterSchema{
				Type: "object", Properties: map[string]types.PropertyDef{"path": {Type: "string"}},
			}}
			inner := &agenttest.MockTool{Def: def, Result: "written"}
			var tool types.Tool = inner
			gate := types.Gates(types.PrefixApprovalGate("", "write_"), denyEtc)
			if tt.marked {
				tool = types.WithMarkers(inner, types.Marker{Kind: "human_approval"})
				gate = denyEtc
			}
			provider := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
				agenttest.ToolCallResponse("c1", "write_file", map[string]any{"path": "/tmp/a"}),
				agenttest.TextResponse("done"),
			}}
			a := must.Get(New(Config{Provider: provider, Tools: types.NewToolRegistry(tool), ToolGate: gate}))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("go"))})
			for d := range stream.Deltas() {
				if m, ok := d.(types.MarkerDelta); ok {
					if err := stream.ResolveMarkerErr(m.ToolCallID, Resolution{Approved: true, ModifiedArgs: tt.edit}); err != nil {
						t.Errorf("resolve: %v", err)
					}
				}
			}
			if err := stream.Wait(); err != nil {
				t.Fatalf("run failed: %v", err)
			}
			if ran := inner.CallCount() == 1; ran != tt.wantRun {
				t.Fatalf("tool ran = %v, want %v", ran, tt.wantRun)
			}
			if tt.wantRun {
				if got := inner.Calls[0]["path"]; got != tt.edit["path"] {
					t.Errorf("path = %v, want the edited %v", got, tt.edit["path"])
				}
				return
			}
			results := toolResults(t, a)
			if len(results) != 1 || !results[0].IsError || !strings.Contains(results[0].Text(), tt.wantText) {
				t.Fatalf("tool results = %+v, want an error containing %q", results, tt.wantText)
			}
		})
	}
}

// A provider stream that ends while a tool call is still open was cut short.
// The call must not run, and the turn must not pass for a clean finish.
func TestOpenToolCallAtStreamEndIsTruncation(t *testing.T) {
	tool := &agenttest.MockTool{Def: types.ToolDef{Name: "write"}}
	provider := &agenttest.ScriptedProvider{Responses: [][]types.Delta{{
		types.PartStart{Index: 0, Kind: types.KindToolCall, ID: "c1", Name: "write"},
		types.PartDelta{Index: 0, Args: `{"path":`},
	}}}
	a := must.Get(New(Config{Provider: provider, Tools: types.NewToolRegistry(tool)}))
	stream := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("go"))})
	for range stream.Deltas() {
	}
	if err := stream.Wait(); !errors.Is(err, types.ErrResponseTruncated) {
		t.Fatalf("Wait = %v, want ErrResponseTruncated", err)
	}
	if tool.CallCount() != 0 {
		t.Error("a truncated tool call ran")
	}
}

// decoratedTool stands for a third-party decorator that wraps a marked tool
// without re-marking it.
type decoratedTool struct{ types.Tool }

func (d decoratedTool) Unwrap() types.Tool { return d.Tool }

func TestMarkerFoundThroughDecorator(t *testing.T) {
	for _, approve := range []bool{true, false} {
		t.Run(fmt.Sprintf("approved=%v", approve), func(t *testing.T) {
			inner := &agenttest.MockTool{Def: types.ToolDef{Name: "write"}, Result: "written"}
			tool := decoratedTool{types.WithMarkers(inner, types.Marker{Kind: "human_approval"})}
			provider := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
				agenttest.ToolCallResponse("c1", "write", map[string]any{}),
				agenttest.TextResponse("done"),
			}}
			a := must.Get(New(Config{Provider: provider, Tools: types.NewToolRegistry(tool)}))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("go"))})
			prompts := 0
			for d := range stream.Deltas() {
				if m, ok := d.(types.MarkerDelta); ok {
					prompts++
					if err := stream.ResolveMarkerErr(m.ToolCallID, Resolution{Approved: approve}); err != nil {
						t.Errorf("resolve: %v", err)
					}
				}
			}
			if err := stream.Wait(); err != nil {
				t.Fatal(err)
			}
			if prompts != 1 {
				t.Fatalf("approval prompts = %d, want 1", prompts)
			}
			if ran := inner.CallCount() == 1; ran != approve {
				t.Fatalf("tool ran = %v, want %v", ran, approve)
			}
		})
	}
}
