package types

import (
	"testing"
	"time"
)

func TestDeltaTypes(t *testing.T) {
	// Verify all concrete types satisfy the Delta interface.
	deltas := []Delta{
		PartStart{Index: 0, Kind: KindText},
		PartDelta{Index: 0, Text: "hello"},
		PartEnd{Index: 0},
		PartStart{Index: 1, Kind: KindToolCall, ID: "tc-1", Name: "greet"},
		PartDelta{Index: 1, Args: `{"name":"Alice"}`},
		PartEnd{Index: 1, Part: ToolCallPart{ID: "tc-1", Name: "greet", Arguments: map[string]any{"name": "Alice"}}},
		ToolExecStartDelta{ToolCallID: "tc-1", Name: "greet"},
		ToolExecDelta{ToolCallID: "tc-1", Inner: PartDelta{Index: 2, Text: "hi"}},
		ToolExecEndDelta{ToolCallID: "tc-1", Result: "done"},
		MarkerDelta{ToolCallID: "tc-1", ToolName: "danger"},
		ErrorDelta{Error: ErrToolNotFound},
		DoneDelta{},
		FeedbackDelta{TargetNodeID: "n-1", Rating: RatingPositive, Comment: "good"},
		UsageDelta{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, Latency: time.Second},
		PartStart{Index: 3, Kind: KindThinking},
		PartDelta{Index: 3, Thinking: "let me think..."},
		PartDelta{Index: 3, Signature: "sig-opaque"}, PartEnd{Index: 3},
	}

	for i, d := range deltas {
		if d == nil {
			t.Errorf("delta[%d] is nil", i)
		}
		// isDelta() is unexported but called implicitly through interface satisfaction
	}

	if len(deltas) != 18 {
		t.Errorf("expected 18 deltas, got %d", len(deltas))
	}
}

func TestPartDeltaValidate(t *testing.T) {
	tests := []struct {
		d  PartDelta
		ok bool
	}{
		{PartDelta{Index: 0, Text: "hello"}, true},
		{PartDelta{Index: 2, Data: []byte{1}}, true},
		{PartDelta{Index: 0}, false},
		{PartDelta{Index: 0, Text: "a", Args: "b"}, false},
		{PartDelta{Index: -1, Text: "a"}, false},
	}
	for _, tt := range tests {
		if err := tt.d.Validate(); (err == nil) != tt.ok {
			t.Errorf("Validate(%+v) = %v, want ok %v", tt.d, err, tt.ok)
		}
	}
}

func TestToolExecDeltaNesting(t *testing.T) {
	inner := PartDelta{Index: 0, Text: "nested"}
	outer := ToolExecDelta{ToolCallID: "tc-1", Inner: inner}

	if tc, ok := outer.Inner.(PartDelta); !ok {
		t.Error("Inner is not PartDelta")
	} else if tc.Text != "nested" {
		t.Errorf("Inner.Text = %q, want %q", tc.Text, "nested")
	}
}

func TestUsageDeltaFields(t *testing.T) {
	d := UsageDelta{
		PromptTokens:     100,
		CompletionTokens: 50,
		TotalTokens:      150,
		Latency:          2 * time.Second,
	}
	if d.TotalTokens != 150 {
		t.Errorf("TotalTokens = %d, want 150", d.TotalTokens)
	}
	if d.Latency != 2*time.Second {
		t.Errorf("Latency = %v, want 2s", d.Latency)
	}
}
