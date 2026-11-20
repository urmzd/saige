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

// Interleaved streams can grow the block slice while a text builder is in
// use. Live snapshots retain their prefix and expose tool input only at stop.
func TestFolderSnapshotsWhileStreaming(t *testing.T) {
	f := NewFolder()
	f.Add(ContentBlockStart{ContentBlock: Block{Type: BlockText, Text: "a"}})
	f.Add(ContentBlockDelta{Delta: BlockDelta{Type: deltaText, Text: "b"}})
	f.Add(ContentBlockStart{Index: 1, ContentBlock: Block{Type: BlockThinking, Thinking: "x"}})
	f.Add(ContentBlockDelta{Index: 1, Delta: BlockDelta{Type: deltaThinking, Thinking: "y"}})
	f.Add(ContentBlockStart{Index: 2, ContentBlock: Block{Type: BlockServerToolUse, Input: json.RawMessage("{}")}})
	f.Add(ContentBlockDelta{Index: 2, Delta: BlockDelta{Type: deltaInputJSON, PartialJSON: `{"a":`}})
	before := f.Message(0)

	f.Add(ContentBlockDelta{Delta: BlockDelta{Type: deltaText, Text: "c"}})
	f.Add(ContentBlockDelta{Index: 1, Delta: BlockDelta{Type: deltaThinking, Thinking: "z"}})
	f.Add(ContentBlockDelta{Index: 2, Delta: BlockDelta{Type: deltaInputJSON, PartialJSON: "1}"}})
	f.Add(ContentBlockStop{Index: 2})
	after := f.Message(0)
	if before.Content[0].Text != "ab" || before.Content[1].Thinking != "xy" || string(before.Content[2].Input) != "{}" {
		t.Fatalf("early snapshot changed: %+v", before.Content)
	}
	if after.Content[0].Text != "abc" || after.Content[1].Thinking != "xyz" || string(after.Content[2].Input) != `{"a":1}` {
		t.Fatalf("later snapshot: %+v", after.Content)
	}

	f.Add(ContentBlockStart{Index: 2, ContentBlock: Block{Type: BlockServerToolUse, Input: json.RawMessage("{}")}})
	f.Add(ContentBlockDelta{Index: 2, Delta: BlockDelta{Type: deltaInputJSON}})
	f.Add(ContentBlockStop{Index: 2})
	if in := f.Message(0).Content[2].Input; in == nil || len(in) != 0 {
		t.Fatalf("empty fragment should replace the initial input, got %q", in)
	}
}
