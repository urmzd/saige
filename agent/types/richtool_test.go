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
	return r.Text(), err
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
	if r.Text() != "plain" || len(r.Parts) != 1 || r.HasMedia() {
		t.Errorf("TextResult = %+v, want one text part", r)
	}
}

func TestImageResult(t *testing.T) {
	r := ImageResult("caption", MediaJPEG, []byte{0xff, 0xd8})
	if r.Text() != "caption" {
		t.Errorf("text = %q", r.Text())
	}
	if len(r.Parts) != 2 {
		t.Fatalf("parts = %d, want 2 (text + image)", len(r.Parts))
	}
	img, ok := r.Parts[1].(ImagePart)
	if _, isText := r.Parts[0].(TextPart); !isText || !ok {
		t.Fatalf("part kinds = %T, %T", r.Parts[0], r.Parts[1])
	}
	if img.Source.MediaType != MediaJPEG || len(img.Source.Inline) != 2 || img.Source.Digest == "" {
		t.Errorf("image part = %+v", img)
	}
}

func TestToolResultTextJoinsTextAndJSON(t *testing.T) {
	r := ToolResult{Parts: []ToolOutputPart{Text("a"), Image(Bytes(MediaPNG, []byte{1})), JSONPart{JSON: []byte(`{"k":1}`)}}}
	if got := r.Text(); got != "a\n{\"k\":1}" {
		t.Errorf("Text() = %q", got)
	}
	if !r.HasMedia() {
		t.Error("HasMedia() = false")
	}
}

func TestToolResultPartJSONOmitsData(t *testing.T) {
	c := ToolOK("id1", Text("ok"), ImagePart{Source: Source{MediaType: MediaPNG, URI: "mem://1", Filename: "chart.png", Inline: []byte{1, 2, 3}}})
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var asMap map[string]any
	_ = json.Unmarshal(raw, &asMap)
	parts, _ := asMap["parts"].([]any)
	if len(parts) != 2 {
		t.Fatalf("parts in JSON = %d, want 2", len(parts))
	}
	src := parts[1].(map[string]any)["source"].(map[string]any)
	if _, hasData := src["data"]; hasData {
		t.Error("inline bytes must not be persisted")
	}
	if src["uri"] != "mem://1" || src["media_type"] != "image/png" {
		t.Errorf("metadata missing in JSON: %+v", src)
	}

	// Round-trips with the bytes dropped and metadata preserved.
	var back ToolResultPart
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	img, ok := back.Parts[1].(ImagePart)
	if !ok || img.Source.URI != "mem://1" || img.Source.Inline != nil {
		t.Fatalf("unmarshal parts = %+v", back.Parts)
	}
}
