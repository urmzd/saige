package types

import (
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
