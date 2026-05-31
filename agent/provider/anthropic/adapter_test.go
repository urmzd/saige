package anthropic

import (
	"encoding/base64"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestToToolResultBlockBackCompat(t *testing.T) {
	// No rich Blocks → take the plain NewToolResultBlock path.
	got := toToolResultBlock(types.ToolResultContent{ToolCallID: "t1", Text: "plain", IsError: false})
	if got.OfToolResult == nil {
		t.Fatal("expected an OfToolResult union")
	}
	if got.OfToolResult.ToolUseID != "t1" {
		t.Errorf("ToolUseID = %q, want t1", got.OfToolResult.ToolUseID)
	}
}

func TestToToolResultBlockTextAndImage(t *testing.T) {
	data := []byte{0x89, 0x50, 0x4e, 0x47}
	c := types.ToolResultContent{
		ToolCallID: "t2",
		Text:       "see image",
		Blocks: []types.ToolResultBlock{
			{Kind: types.ToolResultBlockText, Text: "see image"},
			{Kind: types.ToolResultBlockImage, MediaType: types.MediaPNG, Data: data},
		},
	}
	got := toToolResultBlock(c)
	if got.OfToolResult == nil {
		t.Fatal("expected OfToolResult")
	}
	content := got.OfToolResult.Content
	if len(content) != 2 {
		t.Fatalf("content blocks = %d, want 2", len(content))
	}
	if content[0].OfText == nil || content[0].OfText.Text != "see image" {
		t.Errorf("block 0 = %+v, want text 'see image'", content[0])
	}
	if content[1].OfImage == nil {
		t.Fatal("block 1 should be an image")
	}
	wantB64 := base64.StdEncoding.EncodeToString(data)
	if content[1].OfImage.Source.OfBase64.Data != wantB64 {
		t.Errorf("image data not base64-encoded correctly")
	}
}

func TestToToolResultBlockPDFDocument(t *testing.T) {
	c := types.ToolResultContent{
		ToolCallID: "t3",
		Text:       "report",
		Blocks: []types.ToolResultBlock{
			{Kind: types.ToolResultBlockFile, MediaType: types.MediaPDF, Data: []byte("%PDF-1.4")},
		},
	}
	content := toToolResultBlock(c).OfToolResult.Content
	if len(content) != 1 || content[0].OfDocument == nil {
		t.Fatalf("expected one document block, got %+v", content)
	}
}

func TestToToolResultBlockUnsupportedFileFallsBackToText(t *testing.T) {
	c := types.ToolResultContent{
		ToolCallID: "t4",
		Text:       "data",
		Blocks: []types.ToolResultBlock{
			{Kind: types.ToolResultBlockFile, MediaType: types.MediaCSV, Filename: "data.csv", Data: []byte("a,b")},
		},
	}
	content := toToolResultBlock(c).OfToolResult.Content
	if len(content) != 1 || content[0].OfText == nil {
		t.Fatalf("expected a text placeholder for unsupported file, got %+v", content)
	}
}

func TestToToolResultBlockJSON(t *testing.T) {
	c := types.ToolResultContent{
		ToolCallID: "t5",
		Text:       "{...}",
		Blocks: []types.ToolResultBlock{
			{Kind: types.ToolResultBlockJSON, JSON: []byte(`{"n":1}`)},
		},
	}
	content := toToolResultBlock(c).OfToolResult.Content
	if len(content) != 1 || content[0].OfText == nil || content[0].OfText.Text != `{"n":1}` {
		t.Fatalf("expected JSON serialized to text, got %+v", content)
	}
}
