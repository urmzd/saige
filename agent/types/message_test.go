package types

import "testing"

func TestMessageRoles(t *testing.T) {
	tests := []struct {
		msg  Message
		want Role
	}{
		{SystemMessage{}, RoleSystem},
		{UserMessage{}, RoleUser},
		{AssistantMessage{}, RoleAssistant},
	}
	for _, tt := range tests {
		if got := tt.msg.Role(); got != tt.want {
			t.Errorf("%T.Role() = %q, want %q", tt.msg, got, tt.want)
		}
	}
}

func TestNewSystemMessage(t *testing.T) {
	msg := SystemMsg(Text("you are helpful"))
	if msg.Role() != RoleSystem {
		t.Errorf("Role = %q, want system", msg.Role())
	}
	if len(msg.Parts) != 1 {
		t.Fatalf("Content len = %d, want 1", len(msg.Parts))
	}
	tc, ok := msg.Parts[0].(TextPart)
	if !ok {
		t.Fatal("Content[0] is not TextContent")
	}
	if tc.Text != "you are helpful" {
		t.Errorf("Text = %q, want %q", tc.Text, "you are helpful")
	}
}

func TestNewUserMessage(t *testing.T) {
	msg := UserMsg(Text("hello"))
	if msg.Role() != RoleUser {
		t.Errorf("Role = %q, want user", msg.Role())
	}
	tc, ok := msg.Parts[0].(TextPart)
	if !ok {
		t.Fatal("Content[0] is not TextContent")
	}
	if tc.Text != "hello" {
		t.Errorf("Text = %q", tc.Text)
	}
}

func TestNewToolResultMessage(t *testing.T) {
	msg := ToolResults(
		ToolResultPart{CallID: "tc-1", Parts: []ToolOutputPart{Text("result1")}},
		ToolResultPart{CallID: "tc-2", Parts: []ToolOutputPart{Text("result2")}},
	)
	if msg.Role() != RoleSystem {
		t.Errorf("Role = %q, want system", msg.Role())
	}
	if len(msg.Parts) != 2 {
		t.Fatalf("Content len = %d, want 2", len(msg.Parts))
	}
}
