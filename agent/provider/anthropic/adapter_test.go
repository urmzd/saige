package anthropic

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestToAnthropicParamsPDFIsNativeDocument(t *testing.T) {
	pdf := []byte("%PDF-1.4 fake")
	msgs := []types.Message{types.NewUserMessageWithFiles("summarize this",
		types.FileContent{MediaType: types.MediaPDF, Data: pdf, Filename: "paper.pdf"})}

	_, out := toAnthropicParams(msgs)
	if len(out) != 1 || len(out[0].Content) != 2 {
		t.Fatalf("messages = %+v, want one user message with 2 blocks", out)
	}
	doc := out[0].Content[1].OfDocument
	if doc == nil {
		t.Fatal("PDF FileContent must map to a native document block, not text")
	}
	if doc.Source.OfBase64 == nil || doc.Source.OfBase64.Data != base64.StdEncoding.EncodeToString(pdf) {
		t.Errorf("document source = %+v, want base64 PDF bytes", doc.Source)
	}
}

func TestToAnthropicParamsNonNativeFileFallsBackToText(t *testing.T) {
	msgs := []types.Message{types.NewUserMessageWithFiles("",
		types.FileContent{MediaType: types.MediaCSV, Data: []byte("a,b"), Filename: "data.csv"})}

	_, out := toAnthropicParams(msgs)
	if len(out) != 1 || len(out[0].Content) != 1 {
		t.Fatalf("messages = %+v, want one user message with 1 block", out)
	}
	if out[0].Content[0].OfText == nil {
		t.Fatalf("non-native file should degrade to text, got %+v", out[0].Content[0])
	}
}

func TestContentSupportClaimsMatchMapping(t *testing.T) {
	// ContentSupport must only claim types the adapter maps natively.
	support := (&Adapter{}).ContentSupport()
	for _, mt := range []types.MediaType{types.MediaJPEG, types.MediaPNG, types.MediaGIF, types.MediaWebP, types.MediaPDF} {
		if !support.Supports(mt) {
			t.Errorf("expected native support for %s", mt)
		}
	}
	if support.Supports(types.MediaCSV) {
		t.Error("CSV must not be claimed native")
	}
}

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

// TestEmptySystemPromptIsOmitted checks that blank system text never becomes
// an empty text block, which the API rejects with 400, and that a request
// with no system text left sends no system field at all.
func TestEmptySystemPromptIsOmitted(t *testing.T) {
	for _, tc := range []struct {
		name string
		msgs []types.Message
		want int // system blocks expected; 0 means the field is absent
	}{
		{"empty", []types.Message{types.NewSystemMessage(""), types.NewUserMessage("hi")}, 0},
		{"whitespace", []types.Message{types.NewSystemMessage(" \n\t"), types.NewUserMessage("hi")}, 0},
		{"blank beside real text", []types.Message{types.NewSystemMessage(""), types.NewSystemMessage("rules"), types.NewUserMessage("hi")}, 1},
		{"no system message", []types.Message{types.NewUserMessage("hi")}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, bodies := captureServer(t)
			a := NewAdapter("k", "claude-haiku-5-5", WithBaseURL(server.URL))
			ch, err := a.ChatStream(context.Background(), tc.msgs, nil)
			if err != nil {
				t.Fatal(err)
			}
			drain(ch)
			system, present := (*bodies)[0]["system"]
			if tc.want == 0 {
				if present {
					t.Fatalf("system = %v, want the field absent", system)
				}
				return
			}
			blocks, _ := system.([]any)
			if len(blocks) != tc.want {
				t.Fatalf("system = %v, want %d block(s)", system, tc.want)
			}
			for _, b := range blocks {
				if b.(map[string]any)["text"] == "" {
					t.Fatalf("empty system block sent: %v", system)
				}
			}
		})
	}
}
