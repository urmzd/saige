package agent

import (
	"bytes"
	"context"
	"testing"

	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/agent/workspace"
)

// TestCommitExternalizesMedia checks that media bytes committed to the tree
// are stored in the workspace, so the stored node reads back with a
// reference instead of elided.
func TestCommitExternalizesMedia(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewMemory()
	prov := &toolCallProvider{toolName: "chart", toolID: "c1", toolArgs: map[string]any{}, response: "all done"}
	tool := &richToolMock{name: "chart", res: types.ImageResult("here is the chart", types.MediaPNG, []byte{1, 2, 3})}
	a := NewAgent(AgentConfig{Provider: prov, Tools: types.NewToolRegistry(tool), SystemPrompt: "s"}, WithWorkspace(ws))
	collectDeltas(a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("draw"))}))

	msgs, err := a.Tree().FlattenBranch("main")
	if err != nil {
		t.Fatal(err)
	}
	var stored []types.Source
	for _, m := range msgs {
		for _, r := range types.Each[types.ToolResultPart](m) {
			for _, o := range r.Parts {
				if src, ok := types.SourceOf(o); ok {
					stored = append(stored, src)
				}
			}
		}
	}
	if len(stored) != 1 {
		t.Fatalf("tool result media = %d, want 1", len(stored))
	}
	src := stored[0]
	if !bytes.Equal(src.Inline, []byte{1, 2, 3}) || src.Ref == "" || src.Digest != workspace.Digest([]byte{1, 2, 3}) {
		t.Fatalf("committed source = %+v", src)
	}
	ref, err := workspace.ParseRef(src.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := ws.Read(ctx, ref, 0, 0); err != nil || !bytes.Equal(data, []byte{1, 2, 3}) {
		t.Fatalf("workspace holds %v, %v", data, err)
	}
	// What a store keeps: the reference and digest, not the bytes.
	raw, err := tree.MarshalMessage(types.UserToolResults(types.ToolOK("c1", types.Image(src))))
	if err != nil {
		t.Fatal(err)
	}
	back, err := tree.UnmarshalMessage(types.RoleUser, raw)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := types.SourceOf(back.(types.UserMessage).Parts[0].(types.ToolResultPart).Parts[0])
	if got.Elided() || got.Ref != src.Ref || len(got.Inline) != 0 {
		t.Fatalf("stored source = %+v", got)
	}
}

func TestExternalizeWithoutWorkspace(t *testing.T) {
	a := NewAgent(AgentConfig{Provider: &toolCallProvider{}, SystemPrompt: "s"})
	in := types.UserMsg(types.Image(types.Bytes(types.MediaPNG, []byte{7})))
	out := a.externalize(context.Background(), in)
	src, _ := types.SourceOf(out.(types.UserMessage).Parts[0])
	if src.Ref != "" || src.Digest == "" {
		t.Fatalf("source = %+v", src)
	}
}

func TestExternalizeReadOnlyWorkspaceAndNesting(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewMemory()
	a := NewAgent(AgentConfig{Provider: &toolCallProvider{}, SystemPrompt: "s"}, WithWorkspace(ws.View(true)))
	in := types.AssistantMsg(types.ServerToolResultPart{CallID: "s", Outputs: []types.Part{types.ImageOutPart{Source: types.Bytes(types.MediaPNG, []byte{8})}}})
	if src, _ := types.SourceOf(a.externalize(ctx, in).(types.AssistantMessage).Parts[0].(types.ServerToolResultPart).Outputs[0]); src.Ref != "" {
		t.Fatalf("read-only workspace wrote %+v", src)
	}
	a = NewAgent(AgentConfig{Provider: &toolCallProvider{}, SystemPrompt: "s"}, WithWorkspace(ws))
	out := a.externalize(ctx, in).(types.AssistantMessage)
	if src, _ := types.SourceOf(out.Parts[0].(types.ServerToolResultPart).Outputs[0]); src.Ref == "" {
		t.Fatal("server tool output was not stored")
	}
	if src, _ := types.SourceOf(in.Parts[0].(types.ServerToolResultPart).Outputs[0]); src.Ref != "" {
		t.Fatal("externalize changed its input")
	}
}
