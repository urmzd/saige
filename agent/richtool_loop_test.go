package agent

import (
	"context"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

type richToolMock struct {
	name string
	res  types.ToolResult
}

func (t *richToolMock) Definition() types.ToolDef { return types.ToolDef{Name: t.name} }
func (t *richToolMock) Execute(ctx context.Context, args map[string]any) (string, error) {
	r, err := t.ExecuteRich(ctx, args)
	return r.Text(), err
}
func (t *richToolMock) ExecuteRich(context.Context, map[string]any) (types.ToolResult, error) {
	return t.res, nil
}

func TestRichToolFlowsThroughLoop(t *testing.T) {
	prov := &toolCallProvider{toolName: "chart", toolID: "c1", toolArgs: map[string]any{}, response: "all done"}
	tool := &richToolMock{name: "chart", res: types.ImageResult("here is the chart", types.MediaPNG, []byte{1, 2, 3})}

	a := NewAgent(AgentConfig{Provider: prov, Tools: types.NewToolRegistry(tool), SystemPrompt: "s"})
	deltas := collectDeltas(a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("draw"))}))

	// The terminal tool-exec delta carries both the text projection and Parts.
	ends := collectDeltasByType[types.ToolExecEndDelta](deltas)
	var end *types.ToolExecEndDelta
	for i := range ends {
		if ends[i].ToolCallID == "c1" {
			end = &ends[i]
		}
	}
	if end == nil {
		t.Fatal("no ToolExecEndDelta for c1")
	}
	if end.Result != "here is the chart" {
		t.Errorf("Result = %q, want text projection", end.Result)
	}
	if len(end.Parts) != 2 || end.Parts[1].Kind() != types.KindImage {
		t.Fatalf("Parts = %+v, want text+image", end.Parts)
	}

	// The persisted tool-result message carries the rich Parts.
	msgs, _ := a.Tree().FlattenBranch("main")
	if !toolResultHasImage(msgs) {
		t.Error("expected a persisted ToolResultPart with an image part")
	}
}

func TestPlainToolYieldsNilParts(t *testing.T) {
	prov := &toolCallProvider{toolName: "echo", toolID: "e1", toolArgs: map[string]any{}, response: "ok"}
	tool := &types.ToolFunc{
		Def: types.ToolDef{Name: "echo"},
		Fn:  func(context.Context, map[string]any) (string, error) { return "plain", nil },
	}
	a := NewAgent(AgentConfig{Provider: prov, Tools: types.NewToolRegistry(tool), SystemPrompt: "s"})
	deltas := collectDeltas(a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("hi"))}))

	for _, end := range collectDeltasByType[types.ToolExecEndDelta](deltas) {
		if end.ToolCallID == "e1" {
			if end.Parts != nil {
				t.Errorf("plain tool Parts = %+v, want nil", end.Parts)
			}
			if end.Result != "plain" {
				t.Errorf("Result = %q, want plain", end.Result)
			}
		}
	}
}

func toolResultHasImage(msgs []types.Message) bool {
	for _, m := range msgs {
		if sm, ok := m.(types.SystemMessage); ok {
			for _, c := range sm.Parts {
				if tr, ok := c.(types.ToolResultPart); ok {
					for _, b := range tr.Parts {
						if b.Kind() == types.KindImage {
							return true
						}
					}
				}
			}
		}
	}
	return false
}
