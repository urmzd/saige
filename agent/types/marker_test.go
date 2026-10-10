package types

import (
	"context"
	"errors"
	"testing"
)

// plainTool implements only Tool (no ExecuteRich).
type plainTool struct {
	text string
	err  error
}

func (t *plainTool) Definition() ToolDef { return ToolDef{Name: "plain"} }
func (t *plainTool) Execute(_ context.Context, _ map[string]any) (string, error) {
	return t.text, t.err
}

func TestMarkedToolExecuteRichPassesThroughRichResult(t *testing.T) {
	inner := &fakeRichTool{res: ImageResult("see chart", MediaPNG, []byte{0x1, 0x2})}
	mt := WithMarkers(inner, Marker{Kind: "human_approval"})

	got, err := mt.ExecuteRich(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Text() != "see chart" {
		t.Errorf("Text = %q, want 'see chart'", got.Text())
	}
	if len(got.Parts) != 2 {
		t.Fatalf("Parts = %d, want 2 (rich result must not degrade to text)", len(got.Parts))
	}
	if img, ok := got.Parts[1].(ImagePart); !ok || img.Source.MediaType != MediaPNG {
		t.Errorf("image part = %+v", got.Parts[1])
	}
}

func TestMarkedToolExecuteRichWrapsPlainTool(t *testing.T) {
	mt := WithMarkers(&plainTool{text: "ok"}, Marker{Kind: "audit"})

	got, err := mt.ExecuteRich(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Text() != "ok" || len(got.Parts) != 1 || got.HasMedia() || got.IsError {
		t.Errorf("ExecuteRich = %+v, want one text part", got)
	}
}

func TestMarkedToolExecuteRichPropagatesPlainError(t *testing.T) {
	toolErr := errors.New("denied")
	mt := WithMarkers(&plainTool{err: toolErr})

	_, err := mt.ExecuteRich(context.Background(), nil)
	if !errors.Is(err, toolErr) {
		t.Errorf("error = %v, want %v", err, toolErr)
	}
}

func TestMarkedToolExecuteUnchanged(t *testing.T) {
	// The plain Execute path must behave exactly as before wrapping.
	mt := WithMarkers(&plainTool{text: "legacy"})
	got, err := mt.Execute(context.Background(), nil)
	if err != nil || got != "legacy" {
		t.Errorf("Execute = %q, %v, want 'legacy', nil", got, err)
	}
}
