package types

import (
	"context"
	"encoding/json"
	"testing"
)

// fakeRichTool implements both Tool and RichTool; Execute delegates to ExecuteRich.
type fakeRichTool struct{ res ToolResult }

func (t *fakeRichTool) Definition() ToolDef { return ToolDef{Name: "rich"} }
func (t *fakeRichTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	r, err := t.ExecuteRich(ctx, args)
	return r.Text, err
}
func (t *fakeRichTool) ExecuteRich(_ context.Context, _ map[string]any) (ToolResult, error) {
	return t.res, nil
}

var _ Tool = (*fakeRichTool)(nil)
var _ RichTool = (*fakeRichTool)(nil)

func TestRichToolExecuteDelegates(t *testing.T) {
	rt := &fakeRichTool{res: ImageResult("see chart", MediaPNG, []byte{0x1, 0x2})}
	got, err := rt.Execute(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "see chart" {
		t.Errorf("Execute text projection = %q, want 'see chart'", got)
	}
}

func TestTextResult(t *testing.T) {
	r := TextResult("plain")
	if r.Text != "plain" || r.Blocks != nil {
		t.Errorf("TextResult = %+v, want {Text:plain, Blocks:nil}", r)
	}
}

func TestImageResult(t *testing.T) {
	r := ImageResult("caption", MediaJPEG, []byte{0xff, 0xd8})
	if r.Text != "caption" {
		t.Errorf("text = %q", r.Text)
	}
	if len(r.Blocks) != 2 {
		t.Fatalf("blocks = %d, want 2 (text + image)", len(r.Blocks))
	}
	if r.Blocks[0].Kind != ToolResultBlockText || r.Blocks[1].Kind != ToolResultBlockImage {
		t.Errorf("block kinds = %v, %v", r.Blocks[0].Kind, r.Blocks[1].Kind)
	}
	if r.Blocks[1].MediaType != MediaJPEG || len(r.Blocks[1].Data) != 2 {
		t.Errorf("image block = %+v", r.Blocks[1])
	}
}

func TestToolResultContentJSONOmitsData(t *testing.T) {
	c := ToolResultContent{
		ToolCallID: "id1",
		Text:       "ok",
		Blocks: []ToolResultBlock{
			{Kind: ToolResultBlockImage, MediaType: MediaPNG, URI: "mem://1", Filename: "chart.png", Data: []byte{1, 2, 3}},
		},
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var asMap map[string]any
	_ = json.Unmarshal(raw, &asMap)
	blocks, _ := asMap["blocks"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("blocks in JSON = %d, want 1", len(blocks))
	}
	block := blocks[0].(map[string]any)
	if _, hasData := block["Data"]; hasData {
		t.Error("raw Data must NOT be serialized (json:\"-\")")
	}
	if block["uri"] != "mem://1" || block["media_type"] != "image/png" {
		t.Errorf("metadata missing in JSON: %+v", block)
	}

	// Round-trips with Data dropped, metadata preserved.
	var back ToolResultContent
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Blocks) != 1 || back.Blocks[0].URI != "mem://1" {
		t.Fatalf("unmarshal blocks = %+v", back.Blocks)
	}
	if back.Blocks[0].Data != nil {
		t.Error("Data should be nil after JSON round-trip")
	}
}

func TestToolResultContentLegacyJSONUnmarshal(t *testing.T) {
	// Old persisted JSON has no "blocks" field → Blocks must be nil.
	const legacy = `{"ToolCallID":"x","Text":"hi","IsError":false}`
	var c ToolResultContent
	if err := json.Unmarshal([]byte(legacy), &c); err != nil {
		t.Fatal(err)
	}
	if c.Text != "hi" || c.Blocks != nil {
		t.Errorf("legacy unmarshal = %+v, want Text=hi Blocks=nil", c)
	}
}
