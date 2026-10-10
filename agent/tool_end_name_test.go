package agent

import (
	"context"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

// TestRejectedToolEndCarriesName checks that the terminal ToolExecEndDelta of
// a call the gate refused, or of a call to an unknown tool, names the tool.
func TestRejectedToolEndCarriesName(t *testing.T) {
	for _, tc := range []struct {
		name string
		call string
	}{
		{"gate refused", "delete"},
		{"unknown tool", "missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
				agenttest.ToolCallResponse("c1", tc.call, map[string]any{}),
				agenttest.TextResponse("ok"),
			}}
			tool := &agenttest.MockTool{Def: types.ToolDef{Name: "delete"}, Result: "deleted"}
			a := must.Get(New(Config{
				Provider: script,
				Tools:    types.NewToolRegistry(tool),
				ToolGate: types.GateFunc(func(context.Context, types.ToolDef, map[string]any) types.GateDecision {
					return types.Deny("not allowed")
				}),
			}))
			stream := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("go"))})
			deltas := agenttest.CollectDeltas(stream.Deltas())
			if err := stream.Wait(); err != nil {
				t.Fatal(err)
			}
			var end *types.ToolExecEndDelta
			for _, d := range deltas {
				if e, ok := d.(types.ToolExecEndDelta); ok && e.ToolCallID == "c1" {
					end = &e
				}
			}
			if end == nil || end.Error == "" {
				t.Fatalf("end = %+v, want an error result", end)
			}
			if end.Name != tc.call {
				t.Fatalf("end name = %q, want %q", end.Name, tc.call)
			}
			if tool.CallCount() != 0 {
				t.Fatal("a refused call must not run")
			}
		})
	}
}
