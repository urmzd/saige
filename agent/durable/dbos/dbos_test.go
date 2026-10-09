package dbos

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"testing"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

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
// survive gob: a common real-world tool schema shape.
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

// TestPayloadsGobEncodeAsInterface guards the wrapper-type registrations. The
// DBOS gob serializer encodes workflow inputs/outputs and step results as
// interface values, and gob only requires Register for the interface path:
// encoding the concrete type directly (the tests above) would not catch a
// missing registration.
func TestPayloadsGobEncodeAsInterface(t *testing.T) {
	payloads := []any{
		RunInput{Messages: []types.Message{types.NewUserMessage("hi")}, Branch: "main"},
		RunOutput{Final: &types.AssistantMessage{Content: []types.AssistantContent{types.TextContent{Text: "ok"}}}},
		types.StepResult{Kind: types.StepKindLLM},
	}
	for _, p := range payloads {
		var buf bytes.Buffer
		v := p
		if err := gob.NewEncoder(&buf).Encode(&v); err != nil {
			t.Errorf("encode %T as interface: %v", p, err)
			continue
		}
		var back any
		if err := gob.NewDecoder(&buf).Decode(&back); err != nil {
			t.Errorf("decode %T as interface: %v", p, err)
		}
	}
}

// TestPendingResult checks the mapping from GetEvent to PendingApproval: an
// event that was never set (a DBOS timeout) means "not waiting", not a failure.
func TestPendingResult(t *testing.T) {
	req := types.ApprovalRequest{ID: "i1"}
	other := errors.New("db down")
	for _, tc := range []struct {
		name    string
		req     types.ApprovalRequest
		err     error
		wantOK  bool
		wantErr error
	}{
		{name: "pending", req: req, wantOK: true},
		{name: "decided clears the request", req: types.ApprovalRequest{}},
		{name: "never set times out", err: &dbos.DBOSError{Code: dbos.TimeoutError, Message: "no event found"}},
		{name: "wrapped timeout", err: fmt.Errorf("get event: %w", &dbos.DBOSError{Code: dbos.TimeoutError})},
		{name: "other error", err: other, wantErr: other},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := pendingResult(tc.req, tc.err)
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got.ID != req.ID {
				t.Errorf("req = %+v, want %+v", got, req)
			}
		})
	}
}
