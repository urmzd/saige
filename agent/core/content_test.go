package core

import "testing"

func TestContentRoleInterfaces(t *testing.T) {
	// TextContent satisfies all three roles
	var _ SystemContent = TextContent{Text: "sys"}
	var _ UserContent = TextContent{Text: "usr"}
	var _ AssistantContent = TextContent{Text: "asst"}

	// ToolUseContent is assistant-only
	var _ AssistantContent = ToolUseContent{ID: "1", Name: "test"}

	// ToolResultContent is system and user
	var _ SystemContent = ToolResultContent{ToolCallID: "1", Text: "ok"}
	var _ UserContent = ToolResultContent{ToolCallID: "1", Text: "ok"}

	// ConfigContent is system and user
	var _ SystemContent = ConfigContent{Model: "gpt-4"}
	var _ UserContent = ConfigContent{Model: "gpt-4"}

	// FileContent is user-only
	var _ UserContent = FileContent{URI: "file:///test.txt"}

	// FeedbackContent is user-only
	var _ UserContent = FeedbackContent{TargetNodeID: "n-1", Rating: RatingPositive}
}

func TestMediaTypes(t *testing.T) {
	types := []MediaType{
		MediaJPEG, MediaPNG, MediaGIF, MediaWebP, MediaPDF,
		MediaCSV, MediaMP3, MediaWAV, MediaMP4,
		MediaHTML, MediaText, MediaJSON,
	}
	seen := make(map[MediaType]bool)
	for _, mt := range types {
		if seen[mt] {
			t.Errorf("duplicate MediaType: %s", mt)
		}
		seen[mt] = true
		if mt == "" {
			t.Error("empty MediaType")
		}
	}
}

func TestRatingValues(t *testing.T) {
	if RatingPositive != 1 {
		t.Errorf("RatingPositive = %d, want 1", RatingPositive)
	}
	if RatingNegative != -1 {
		t.Errorf("RatingNegative = %d, want -1", RatingNegative)
	}
}
