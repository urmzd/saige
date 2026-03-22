package core

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
	msg := NewSystemMessage("you are helpful")
	if msg.Role() != RoleSystem {
		t.Errorf("Role = %q, want system", msg.Role())
	}
	if len(msg.Content) != 1 {
		t.Fatalf("Content len = %d, want 1", len(msg.Content))
	}
	tc, ok := msg.Content[0].(TextContent)
	if !ok {
		t.Fatal("Content[0] is not TextContent")
	}
	if tc.Text != "you are helpful" {
		t.Errorf("Text = %q, want %q", tc.Text, "you are helpful")
	}
}

func TestNewUserMessage(t *testing.T) {
	msg := NewUserMessage("hello")
	if msg.Role() != RoleUser {
		t.Errorf("Role = %q, want user", msg.Role())
	}
	tc, ok := msg.Content[0].(TextContent)
	if !ok {
		t.Fatal("Content[0] is not TextContent")
	}
	if tc.Text != "hello" {
		t.Errorf("Text = %q", tc.Text)
	}
}

func TestNewToolResultMessage(t *testing.T) {
	msg := NewToolResultMessage(
		ToolResultContent{ToolCallID: "tc-1", Text: "result1"},
		ToolResultContent{ToolCallID: "tc-2", Text: "result2"},
	)
	if msg.Role() != RoleSystem {
		t.Errorf("Role = %q, want system", msg.Role())
	}
	if len(msg.Content) != 2 {
		t.Fatalf("Content len = %d, want 2", len(msg.Content))
	}
}

func TestNewFileMessage(t *testing.T) {
	msg := NewFileMessage("file:///test.pdf", MediaPDF)
	if len(msg.Content) != 1 {
		t.Fatalf("Content len = %d, want 1", len(msg.Content))
	}
	fc, ok := msg.Content[0].(FileContent)
	if !ok {
		t.Fatal("Content[0] is not FileContent")
	}
	if fc.URI != "file:///test.pdf" {
		t.Errorf("URI = %q", fc.URI)
	}
	if fc.MediaType != MediaPDF {
		t.Errorf("MediaType = %q, want %q", fc.MediaType, MediaPDF)
	}
}

func TestNewUserMessageWithFiles(t *testing.T) {
	msg := NewUserMessageWithFiles("check this",
		FileContent{URI: "file:///a.jpg", MediaType: MediaJPEG},
		FileContent{URI: "file:///b.png", MediaType: MediaPNG},
	)
	if len(msg.Content) != 3 {
		t.Fatalf("Content len = %d, want 3", len(msg.Content))
	}
}
