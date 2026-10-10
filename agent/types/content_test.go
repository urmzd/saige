package types

import "testing"

func TestContentRoleInterfaces(t *testing.T) {
	// TextPart satisfies all three roles
	var _ SystemPart = TextPart{Text: "sys"}
	var _ UserPart = TextPart{Text: "usr"}
	var _ AssistantPart = TextPart{Text: "asst"}

	// ToolCallPart is assistant-only
	var _ AssistantPart = ToolCallPart{ID: "1", Name: "test"}

	// ToolResultPart is system and user
	var _ SystemPart = ToolResultPart{CallID: "1", Parts: []ToolOutputPart{Text("ok")}}
	var _ UserPart = ToolResultPart{CallID: "1", Parts: []ToolOutputPart{Text("ok")}}

	// ConfigPart is system and user
	var _ SystemPart = ConfigPart{Model: "gpt-4"}
	var _ UserPart = ConfigPart{Model: "gpt-4"}

	// Media parts are user input and tool output
	var _ UserPart = Document(URL("file:///test.txt"))
	var _ ToolOutputPart = Image(Bytes(MediaPNG, []byte{1}))

	// FeedbackPart is user-only
	var _ UserPart = FeedbackPart{TargetNodeID: "n-1", Rating: RatingPositive}
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
