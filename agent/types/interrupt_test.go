package types

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestInterruptID(t *testing.T) {
	base := InterruptID("run", []string{"a", "b"}, "marker", "c1")
	if base != InterruptID("run", []string{"a", "b"}, "marker", "c1") {
		t.Fatal("not deterministic")
	}
	variants := map[string]string{
		"run":   InterruptID("run2", []string{"a", "b"}, "marker", "c1"),
		"path":  InterruptID("run", []string{"a"}, "marker", "c1"),
		"phase": InterruptID("run", []string{"a", "b"}, "gate", "c1"),
		"call":  InterruptID("run", []string{"a", "b"}, "marker", "c2"),
		// Field boundaries matter: ("ab","") must not equal ("a","b").
		"split": InterruptID("run", []string{"a", "b"}, "marke", "rc1"),
	}
	for name, id := range variants {
		if id == base {
			t.Errorf("changing %s kept the same ID", name)
		}
	}
}

func TestInterruptExpired(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		expires time.Time
		want    bool
	}{
		{"no expiry", time.Time{}, false},
		{"future", now.Add(time.Second), false},
		{"exactly now", now, true},
		{"past", now.Add(-time.Second), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (Interrupt{ExpiresAt: tt.expires}).Expired(now); got != tt.want {
				t.Errorf("Expired = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsMetadataContent(t *testing.T) {
	tests := []struct {
		c    Part
		want bool
	}{
		{ConfigPart{}, true},
		{HandoffPart{}, true},
		{FeedbackPart{}, true},
		{SteerPart{ID: "s"}, true},
		{TruncationPart{Reason: "max_tokens"}, true},
		{RoutePart{Model: "m"}, true},
		{TextPart{}, false},
		{ToolCallPart{}, false},
		{ServerToolCallPart{}, false},
		{ApprovalPart{}, true},
		{GuardrailPart{}, true},
		{CompactionPart{}, true},
		{ImagePart{}, false},
	}
	for _, tt := range tests {
		if got := IsMetadata(tt.c); got != tt.want {
			t.Errorf("IsMetadata(%T) = %v, want %v", tt.c, got, tt.want)
		}
	}
}

func TestTypedInterruptPayloads(t *testing.T) {
	q, _ := json.Marshal(ClarificationPayload{Question: "which file?"})
	in := Interrupt{ID: "i1", Kind: InterruptClarification, Payload: q}
	got, err := in.Clarification()
	if err != nil || got.Question != "which file?" {
		t.Fatalf("Clarification = %+v, %v", got, err)
	}
	if _, err := in.ToolCall(); !errors.Is(err, ErrInterruptPayload) {
		t.Fatalf("a clarification read as a tool call: %v", err)
	}
	call, _ := json.Marshal(ToolCallPart{ID: "c1", Name: "rm", Arguments: map[string]any{"path": "x"}})
	approval := Interrupt{ID: "i2", Kind: InterruptApproval, Payload: call}
	if tc, err := approval.ToolCall(); err != nil || tc.Name != "rm" || tc.ID != "c1" {
		t.Fatalf("ToolCall = %+v, %v", tc, err)
	}
	if _, err := approval.Clarification(); !errors.Is(err, ErrInterruptPayload) {
		t.Fatal("an approval read as a clarification")
	}
	if _, err := (Interrupt{ID: "i3", Kind: InterruptClarification}).Clarification(); !errors.Is(err, ErrInterruptPayload) {
		t.Fatal("an empty payload decoded")
	}

	reply, err := Answer("i1", "main.go")
	if err != nil {
		t.Fatal(err)
	}
	if s, err := ReplyAnswer[string](reply); err != nil || s != "main.go" {
		t.Fatalf("ReplyAnswer = %q, %v", s, err)
	}
	if _, err := ReplyAnswer[int](reply); !errors.Is(err, ErrInterruptPayload) {
		t.Fatal("a string answer decoded as an int")
	}

	type args struct {
		Path string `json:"path"`
	}
	if _, ok, err := ModifiedArgsAs[args](ApprovalDecision{Approved: true}); ok || err != nil {
		t.Fatal("unmodified arguments reported as modified")
	}
	a, ok, err := ModifiedArgsAs[args](ApprovalDecision{Approved: true, ModifiedArgs: map[string]any{"path": "y"}})
	if !ok || err != nil || a.Path != "y" {
		t.Fatalf("ModifiedArgsAs = %+v, %v, %v", a, ok, err)
	}
}
