package dbos

import (
	"bytes"
	"encoding/gob"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

// TestStepResultGobRoundTrip guards the init() gob registrations: a StepResult
// carrying an AssistantMessage (with all content kinds) and rich tool blocks
// must survive a gob encode/decode, which is how DBOS memoizes step results.
func TestStepResultGobRoundTrip(t *testing.T) {
	msg := types.AssistantMessage{Content: []types.AssistantContent{
		types.TextContent{Text: "hello"},
		types.ToolUseContent{ID: "1", Name: "f", Arguments: map[string]any{"q": "go", "n": 3.0, "ok": true}},
		types.ThinkingContent{Thinking: "reasoning", Signature: "sig"},
	}}
	sr := types.StepResult{
		Kind:       types.StepKindLLM,
		Message:    &msg,
		ToolBlocks: []types.ToolResultBlock{{Kind: types.ToolResultBlockImage, MediaType: types.MediaPNG, Data: []byte{1, 2, 3}}},
	}

	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(sr); err != nil {
		t.Fatalf("encode: %v", err)
	}
	var back types.StepResult
	if err := gob.NewDecoder(&buf).Decode(&back); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if back.Message == nil || len(back.Message.Content) != 3 {
		t.Fatalf("decoded message = %+v", back.Message)
	}
	if txt, ok := back.Message.Content[0].(types.TextContent); !ok || txt.Text != "hello" {
		t.Errorf("content[0] = %+v, want TextContent hello", back.Message.Content[0])
	}
	if len(back.ToolBlocks) != 1 || len(back.ToolBlocks[0].Data) != 3 {
		t.Errorf("tool block bytes not preserved through gob: %+v", back.ToolBlocks)
	}
}

// TestNestedToolArgsGobRoundTrip guards that tool arguments containing JSON
// arrays and nested objects (decoded as []interface{} / map[string]interface{})
// survive gob — a common real-world tool schema shape.
func TestNestedToolArgsGobRoundTrip(t *testing.T) {
	msg := types.AssistantMessage{Content: []types.AssistantContent{
		types.ToolUseContent{ID: "1", Name: "f", Arguments: map[string]any{
			"items": []interface{}{"a", 1.0, true},
			"opts":  map[string]interface{}{"k": "v", "nested": []interface{}{2.0}},
		}},
	}}
	sr := types.StepResult{Kind: types.StepKindLLM, Message: &msg}

	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(sr); err != nil {
		t.Fatalf("encode nested args: %v", err)
	}
	var back types.StepResult
	if err := gob.NewDecoder(&buf).Decode(&back); err != nil {
		t.Fatalf("decode nested args: %v", err)
	}
	tu := back.Message.Content[0].(types.ToolUseContent)
	if _, ok := tu.Arguments["items"].([]interface{}); !ok {
		t.Errorf("array arg not preserved: %+v", tu.Arguments["items"])
	}
}

// TestRunInputGobRoundTrip guards that workflow inputs (which carry sealed
// Message values) round-trip through gob.
func TestRunInputGobRoundTrip(t *testing.T) {
	in := RunInput{
		Messages: []types.Message{
			types.NewSystemMessage("root"),
			types.NewUserMessage("hi"),
		},
		Branch: "main",
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(in); err != nil {
		t.Fatalf("encode: %v", err)
	}
	var back RunInput
	if err := gob.NewDecoder(&buf).Decode(&back); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(back.Messages) != 2 || back.Branch != "main" {
		t.Fatalf("decoded = %+v", back)
	}
}
