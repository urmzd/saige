package tree

import (
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestCompactCount(t *testing.T) {
	user := types.NewUserMessage("u")
	text := types.NewAssistantMessage("a")
	call := types.AssistantMessage{Content: []types.AssistantContent{types.ToolUseContent{ID: "c", Name: "f"}}}
	result := types.NewToolResultMessage(types.ToolResultContent{ToolCallID: "c", Text: "r"})
	tests := []struct {
		name       string
		candidates []types.Message
		want       int
	}{
		{name: "empty", want: 0},
		{name: "one message", candidates: []types.Message{user}, want: 1},
		{name: "plain half", candidates: []types.Message{user, text, user, text}, want: 2},
		{name: "moves past the result of the last summarized call", candidates: []types.Message{user, call, result, call, result}, want: 3},
		{name: "moves past several results", candidates: []types.Message{call, result, result, user, text, user}, want: 3},
		{name: "moves back when forward would take everything", candidates: []types.Message{user, text, call, result}, want: 2},
		{name: "takes everything when nothing else is clean", candidates: []types.Message{call, result}, want: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CompactCount(tt.candidates); got != tt.want {
				t.Fatalf("CompactCount = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestBaseBranchName(t *testing.T) {
	tests := []struct {
		branch types.BranchID
		want   string
	}{
		{"main", "main"},
		{"compact-main-1a2b3c4d", "main"},
		{"compact-compact-main-1a2b3c4d-5e6f7a8b", "main"},
		{"compact-feature-x-1a2b3c4d", "feature-x"},
		{"compact-short", "compact-short"},
	}
	for _, tt := range tests {
		t.Run(string(tt.branch), func(t *testing.T) {
			if got := baseBranchName(tt.branch); got != tt.want {
				t.Fatalf("baseBranchName(%q) = %q, want %q", tt.branch, got, tt.want)
			}
		})
	}
}
