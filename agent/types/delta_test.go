package types

import (
	"testing"
	"time"
)

func TestDeltaTypes(t *testing.T) {
	// Verify all concrete types satisfy the Delta interface.
	deltas := []Delta{
		TextStartDelta{},
		TextContentDelta{Content: "hello"},
		TextEndDelta{},
		ToolCallStartDelta{ID: "tc-1", Name: "greet"},
		ToolCallArgumentDelta{Content: `{"name":"Alice"}`},
		ToolCallEndDelta{Arguments: map[string]any{"name": "Alice"}},
		ToolExecStartDelta{ToolCallID: "tc-1", Name: "greet"},
		ToolExecDelta{ToolCallID: "tc-1", Inner: TextContentDelta{Content: "hi"}},
		ToolExecEndDelta{ToolCallID: "tc-1", Result: "done"},
		MarkerDelta{ToolCallID: "tc-1", ToolName: "danger"},
		ErrorDelta{Error: ErrToolNotFound},
		DoneDelta{},
		FeedbackDelta{TargetNodeID: "n-1", Rating: RatingPositive, Comment: "good"},
		UsageDelta{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, Latency: time.Second},
		ThinkingStartDelta{},
		ThinkingContentDelta{Content: "let me think..."},
		ThinkingEndDelta{Signature: "sig-opaque"},
	}

	for i, d := range deltas {
		if d == nil {
			t.Errorf("delta[%d] is nil", i)
		}
		// isDelta() is unexported but called implicitly through interface satisfaction
	}

	if len(deltas) != 17 {
		t.Errorf("expected 17 delta types, got %d", len(deltas))
	}
}

func TestTextContentDeltaContent(t *testing.T) {
	d := TextContentDelta{Content: "hello world"}
	if d.Content != "hello world" {
		t.Errorf("Content = %q, want %q", d.Content, "hello world")
	}
}

func TestToolExecDeltaNesting(t *testing.T) {
	inner := TextContentDelta{Content: "nested"}
	outer := ToolExecDelta{ToolCallID: "tc-1", Inner: inner}

	if tc, ok := outer.Inner.(TextContentDelta); !ok {
		t.Error("Inner is not TextContentDelta")
	} else if tc.Content != "nested" {
		t.Errorf("Inner.Content = %q, want %q", tc.Content, "nested")
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
