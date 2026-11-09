package message

import (
	"encoding/json"
	"testing"
)

// A block is written as Anthropic writes it, so a stock client folds it:
// an opening text or thinking block has its empty field, and a tool call
// its input.
func TestABlockIsWrittenAsAnthropicWritesIt(t *testing.T) {
	for _, tc := range []struct {
		block Block
		want  string
	}{
		{Block{Type: BlockText}, `{"type":"text","text":""}`},
		{Block{Type: BlockThinking}, `{"type":"thinking","thinking":""}`},
		{Block{Type: BlockServerToolUse, ID: "htu_r_1", Name: "agent", Kind: KindAgent, Title: "Review it"}, `{"type":"server_tool_use","id":"htu_r_1","name":"agent","input":{},"kind":"agent","title":"Review it"}`},
		{Block{Type: BlockServerToolResult, ToolUseID: "htu_r_1"}, `{"type":"server_tool_result","tool_use_id":"htu_r_1","content":[]}`},
	} {
		got, err := json.Marshal(tc.block)
		if err != nil || string(got) != tc.want {
			t.Errorf("%s: %s, %v", tc.block.Type, got, err)
		}
		var back Block
		if err := json.Unmarshal(got, &back); err != nil || back.Type != tc.block.Type {
			t.Errorf("%s does not read back: %+v, %v", tc.block.Type, back, err)
		}
	}
	// A folded block keeps its call's status and lane beside its fields.
	c := Content{Block: Block{Type: BlockServerToolUse, ID: "htu_r_1", Name: "agent"}, Status: &ToolStatus{ToolUseID: "htu_r_1", Path: []string{"htu_r_1"}, Status: StatusDone}, Lane: &Lane{AgentRunID: "run_b", Depth: 1, Content: []Content{{Block: Block{Type: BlockText, Text: "Fine."}}}, Done: true}}
	got, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var back Content
	if err := json.Unmarshal(got, &back); err != nil || back.Status == nil || back.Lane == nil || back.Lane.Content[0].Text != "Fine." || back.ID != "htu_r_1" {
		t.Fatalf("content = %s, read back %+v, %v", got, back, err)
	}
}
