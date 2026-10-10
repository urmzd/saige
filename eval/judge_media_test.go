package eval

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

// partsGenerator records what a judge sends with media.
type partsGenerator struct {
	prompts []string
	parts   [][]types.UserPart
	schemas []json.RawMessage
}

func (g *partsGenerator) Generate(_ context.Context, prompt string) (string, error) {
	g.prompts = append(g.prompts, prompt)
	return `{"reasoning":"text only","score":0.5}`, nil
}

func (g *partsGenerator) GenerateParts(_ context.Context, parts []types.UserPart, schema json.RawMessage) (string, error) {
	g.parts = append(g.parts, parts)
	g.schemas = append(g.schemas, schema)
	return `{"reasoning":"saw the image","score":1}`, nil
}

const mediaInput = `{"parts":[{"type":"text","text":"What color is the square?"},{"type":"image","source":{"media_type":"image/png","uri":"file:red.png"}}]}`

func TestJudgeSendsInputMedia(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "red.png"), []byte("red png"), 0o600); err != nil {
		t.Fatal(err)
	}
	gen := &partsGenerator{}
	scorer := NewJudgeScorer(gen, WithJudgeResolvers(map[string]types.Resolver{"file": DirResolver(dir)}))
	obs := Observation{ID: "m1", Input: json.RawMessage(mediaInput), Output: json.RawMessage(`"Red."`)}

	score, err := scorer.Score(context.Background(), obs)
	if err != nil {
		t.Fatal(err)
	}
	if score.Value != 1 || score.Reason != "saw the image" {
		t.Fatalf("score = %+v", score)
	}
	if len(gen.parts) != 1 || len(gen.prompts) != 0 {
		t.Fatalf("calls: %d parts, %d text", len(gen.parts), len(gen.prompts))
	}
	sent := gen.parts[0]
	if len(sent) != 2 {
		t.Fatalf("sent %d parts, want prompt and image", len(sent))
	}
	prompt, ok := sent[0].(types.TextPart)
	if !ok || !strings.Contains(prompt.Text, "What color is the square?") ||
		!strings.Contains(prompt.Text, "[attachment 1: image image/png") || strings.Contains(prompt.Text, `"parts"`) {
		t.Fatalf("prompt = %q", prompt.Text)
	}
	src, _ := types.SourceOf(sent[1])
	if string(src.Inline) != "red png" {
		t.Fatalf("image bytes = %q", src.Inline)
	}
	if string(gen.schemas[0]) != string(JudgeSchema) {
		t.Fatalf("schema = %s", gen.schemas[0])
	}
}

func TestJudgeWithoutPartsGeneratorRefusesMedia(t *testing.T) {
	scorer := NewJudgeScorer(&mockGenerator{response: "SCORE: 1"})
	_, err := scorer.Score(context.Background(), Observation{ID: "m1", Input: json.RawMessage(mediaInput), Output: json.RawMessage(`"Red."`)})
	if !errors.Is(err, ErrJudgeMedia) {
		t.Fatalf("err = %v, want ErrJudgeMedia", err)
	}
}

func TestJudgeTextInputsAreUnchanged(t *testing.T) {
	gen := &partsGenerator{}
	scorer := NewJudgeScorer(gen)
	for _, raw := range []string{`"What is Go?"`, `{"question":"What is Go?"}`} {
		if _, err := scorer.Score(context.Background(), Observation{ID: "t", Input: json.RawMessage(raw), Output: json.RawMessage(`"A language."`)}); err != nil {
			t.Fatal(err)
		}
		last := gen.prompts[len(gen.prompts)-1]
		if !strings.Contains(last, "## Input\n"+raw+"\n") {
			t.Fatalf("input %s rendered as:\n%s", raw, last)
		}
	}
	if len(gen.parts) != 0 {
		t.Fatal("a text input was sent as parts")
	}
	// A parts input without media is judged as text.
	if _, err := scorer.Score(context.Background(), Observation{ID: "p", Input: json.RawMessage(`{"parts":[{"type":"text","text":"Hi"}]}`), Output: json.RawMessage(`"Hello"`)}); err != nil {
		t.Fatal(err)
	}
	if last := gen.prompts[len(gen.prompts)-1]; !strings.Contains(last, "## Input\nHi\n") {
		t.Fatalf("parts input rendered as:\n%s", last)
	}
}

func TestPairwiseJudgeSendsInputMedia(t *testing.T) {
	gen := &partsGenerator{}
	scorer := NewPairwiseJudgeScorer(gen)
	obs := Observation{ID: "m1", Input: json.RawMessage(mediaInput), Output: json.RawMessage(`"Red."`), GroundTruth: json.RawMessage(`"Blue."`)}
	if _, err := scorer.Score(context.Background(), obs); err != nil {
		t.Fatal(err)
	}
	if len(gen.parts) != 2 {
		t.Fatalf("pairwise sent media on %d calls, want both orders", len(gen.parts))
	}
	for _, sent := range gen.parts {
		if src, _ := types.SourceOf(sent[len(sent)-1]); src.URI != "file:red.png" {
			t.Fatalf("media source = %+v", src)
		}
	}
}

func TestProvenanceConversions(t *testing.T) {
	var a, b Provenance
	a.AddConversion("image", "native", "")
	a.AddConversion("document", "extracted", "documents@1")
	a.AddConversion("image", "native", "")
	a.AddConversion("", "native", "")
	if len(a.Conversions) != 2 || a.Conversions[0].Kind != "document" {
		t.Fatalf("conversions = %+v", a.Conversions)
	}
	b.AddConversion("image", "described", "describe@1")
	b.AddConversion("document", "extracted", "documents@1")
	drift := ConfigDrift(a, b)
	if len(drift) != 1 || !strings.Contains(drift[0], "image media: native vs described via describe@1") {
		t.Fatalf("drift = %v", drift)
	}
	if d := ConfigDrift(a, Provenance{}); len(d) != 0 {
		t.Fatalf("a run without conversion provenance drifted: %v", d)
	}
}
