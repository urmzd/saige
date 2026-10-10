package online_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/provider"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/eval"
	"github.com/urmzd/saige/eval/online"
)

// TestLiveJudgeSeesAnImageInput judges a dataset case whose input is an
// image, referenced by a file: uri next to the dataset, on
// claude-haiku-5-5. A right and a wrong answer must score apart, which only
// happens if the judge saw the image. It runs only with SAIGE_LIVE=1 and an
// ANTHROPIC_API_KEY.
func TestLiveJudgeSeesAnImageInput(t *testing.T) {
	if os.Getenv("SAIGE_LIVE") != "1" || os.Getenv("ANTHROPIC_API_KEY") == "" {
		t.Skip("set SAIGE_LIVE=1 and ANTHROPIC_API_KEY to call the provider")
	}
	dir := t.TempDir()
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for x := range 64 {
		for y := range 64 {
			img.Set(x, y, color.RGBA{G: 180, A: 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "square.png"), b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	input := json.RawMessage(`{"parts":[{"type":"text","text":"What color is this square?"},` +
		`{"type":"image","source":{"media_type":"image/png","uri":"file:square.png"}}]}`)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	p, err := provider.Build(ctx, provider.Config{Provider: provider.Anthropic, Model: "claude-haiku-5-5"})
	if err != nil {
		t.Fatal(err)
	}
	budget := types.NewBudget(types.BudgetPolicy{Limit: types.USD(0.05), PerCallCost: types.USD(0.01), MaxRequests: 2})
	judge := eval.NewJudgeScorer(&online.BudgetedGenerator{Provider: p, Budget: budget},
		eval.WithJudgeRubric("Score 1 when the response names the color of the square in the image correctly, 0 when it does not."),
		eval.WithJudgeResolvers(map[string]types.Resolver{"file": eval.DirResolver(dir)}))

	right, err := judge.Score(ctx, eval.Observation{ID: "right", Input: input, Output: json.RawMessage(`"It is green."`)})
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := judge.Score(ctx, eval.Observation{ID: "wrong", Input: input, Output: json.RawMessage(`"It is purple."`)})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("right=%.2f (%s); wrong=%.2f (%s)", right.Value, right.Reason, wrong.Value, wrong.Reason)
	if right.Value < 0.7 || wrong.Value > 0.3 {
		t.Fatalf("the judge did not tell the answers apart: right %.2f, wrong %.2f", right.Value, wrong.Value)
	}
	for _, r := range budget.Breakdown() {
		t.Logf("spend: %s %d in / %d out tokens, %s", r.Model, r.Usage.InputTokens, r.Usage.OutputTokens, r.Cost)
	}
	t.Logf("judge spend %s over %d calls", budget.Spent(), budget.Usage().Requests)
}
