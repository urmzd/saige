package eval

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

type mockGenerator struct {
	response string
}

func (m *mockGenerator) Generate(_ context.Context, _ string) (string, error) {
	return m.response, nil
}

func TestJudgeScorerParsesOutput(t *testing.T) {
	gen := &mockGenerator{response: "REASONING: Good response\nSCORE: 0.85"}

	scorer := NewJudgeScorer(gen)
	obs := Observation{
		ID:     "j1",
		Input:  json.RawMessage(`"What is Go?"`),
		Output: json.RawMessage(`"Go is a programming language."`),
	}

	score, err := scorer.Score(context.Background(), obs)
	if err != nil {
		t.Fatal(err)
	}

	if score.Name != "judge_score" {
		t.Errorf("expected name judge_score, got %s", score.Name)
	}
	assertClose(t, "value", score.Value, 0.85, 0.001)
	if score.Reason != "Good response" {
		t.Errorf("expected reason 'Good response', got %q", score.Reason)
	}
}

func TestJudgeScorerCustomName(t *testing.T) {
	gen := &mockGenerator{response: "REASONING: Fine\nSCORE: 0.5"}

	scorer := NewJudgeScorer(gen, WithJudgeName("custom_judge"))
	obs := Observation{
		ID:     "j2",
		Output: json.RawMessage(`"answer"`),
	}

	score, err := scorer.Score(context.Background(), obs)
	if err != nil {
		t.Fatal(err)
	}

	if score.Name != "custom_judge" {
		t.Errorf("expected name custom_judge, got %s", score.Name)
	}
}

func TestJudgeScorerClampsScore(t *testing.T) {
	gen := &mockGenerator{response: "REASONING: Over\nSCORE: 1.5"}

	scorer := NewJudgeScorer(gen)
	obs := Observation{ID: "j3", Output: json.RawMessage(`"x"`)}

	score, err := scorer.Score(context.Background(), obs)
	if err != nil {
		t.Fatal(err)
	}
	assertClose(t, "clamped", score.Value, 1.0, 0.001)
}

func TestPairwiseJudgeScorer(t *testing.T) {
	gen := &mockGenerator{response: "REASONING: B is better\nSCORE: 0.8"}

	scorer := NewPairwiseJudgeScorer(gen, WithPositionSwap(false))
	obs := Observation{
		ID:          "pw1",
		Input:       json.RawMessage(`"query"`),
		GroundTruth: json.RawMessage(`"response A"`),
		Output:      json.RawMessage(`"response B"`),
	}

	score, err := scorer.Score(context.Background(), obs)
	if err != nil {
		t.Fatal(err)
	}

	if score.Name != "pairwise_judge" {
		t.Errorf("expected pairwise_judge, got %s", score.Name)
	}
	assertClose(t, "value", score.Value, 0.8, 0.001)
}

func TestParseJudgeOutput(t *testing.T) {
	tests := []struct {
		name    string
		reply   string
		want    float64
		reason  string
		wantErr bool
	}{
		{name: "plain", reply: "REASONING: fine\nSCORE: 0.85", want: 0.85, reason: "fine"},
		{name: "fraction", reply: "SCORE: 8/10", want: 0.8},
		{name: "bold label", reply: "**SCORE:** 0.7", want: 0.7},
		{name: "bold label outside colon", reply: "**Score**: 0.6", want: 0.6},
		{name: "equals separator", reply: "Score = 0.7", want: 0.7},
		{name: "trailing period", reply: "SCORE: 0.9.", want: 0.9},
		{name: "trailing note", reply: "SCORE: 0.4 (partial)", want: 0.4},
		{name: "last score wins", reply: "SCORE: 0.1\nSCORE: 0.3", want: 0.3},
		{name: "clamped high", reply: "SCORE: 1.5", want: 1},
		{name: "clamped low", reply: "SCORE: -2", want: 0},
		{name: "missing score", reply: "REASONING: no score line here", reason: "no score line here", wantErr: true},
		{name: "refusal", reply: "I cannot evaluate this.", wantErr: true},
		{name: "truncated", reply: "REASONING: the response is", wantErr: true},
		{name: "not a number", reply: "SCORE: high", wantErr: true},
		{name: "nan", reply: "SCORE: NaN", wantErr: true},
		{name: "infinity", reply: "SCORE: +Inf", wantErr: true},
		{name: "zero denominator", reply: "SCORE: 1/0", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason, err := parseJudgeOutput(tt.reply)
			if tt.wantErr {
				if !errors.Is(err, ErrNoJudgeScore) {
					t.Fatalf("expected ErrNoJudgeScore, got %v (score %v)", err, got)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				assertClose(t, "score", got, tt.want, 0.001)
			}
			if tt.reason != "" && reason != tt.reason {
				t.Errorf("reason: got %q, want %q", reason, tt.reason)
			}
		})
	}
}

func TestJudgeUnparseableReplyIsErroredScore(t *testing.T) {
	gen := &mockGenerator{response: "no score"}
	obs := []Observation{
		{ID: "a", Output: json.RawMessage(`"x"`)},
		{ID: "b", Output: json.RawMessage(`"y"`)},
	}
	ok := NewScorerFunc("ok", func(_ context.Context, _ Observation) (Score, error) {
		return Score{Name: "ok", Value: 1}, nil
	})

	suite, err := Run(context.Background(), "judge", obs, []Scorer{NewJudgeScorer(gen), ok})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range suite.Results {
		if r.Scores[0].Error == "" {
			t.Errorf("case %s: expected an errored judge score, got %+v", r.Observation.ID, r.Scores[0])
		}
	}
	if _, present := suite.Aggregate["judge_score"]; present {
		t.Error("judge_score must be absent from Aggregate when every reply is unparseable")
	}
	if suite.ErroredCases != 2 {
		t.Errorf("ErroredCases: got %d, want 2", suite.ErroredCases)
	}
}

// positionBiasedGenerator always prefers whichever response is shown first
// and records each prompt it was given.
type positionBiasedGenerator struct {
	mu      sync.Mutex
	prompts []string
}

func (g *positionBiasedGenerator) Generate(_ context.Context, prompt string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.prompts = append(g.prompts, prompt)
	return "REASONING: Response A is better\nSCORE: 0.0", nil
}

func TestPairwiseJudgePositionSwap(t *testing.T) {
	obs := Observation{
		ID:          "pw",
		Input:       json.RawMessage(`"query"`),
		GroundTruth: json.RawMessage(`"BASE-ANSWER"`),
		Output:      json.RawMessage(`"EXP-ANSWER"`),
	}

	tests := []struct {
		name       string
		opts       []JudgeOption
		wantValue  float64
		wantCalls  int
		wantStable *bool
	}{
		{name: "swap on by default", wantValue: 0.5, wantCalls: 2, wantStable: boolPtr(false)},
		{name: "swap off", opts: []JudgeOption{WithPositionSwap(false)}, wantValue: 0.0, wantCalls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gen := &positionBiasedGenerator{}
			score, err := NewPairwiseJudgeScorer(gen, tt.opts...).Score(context.Background(), obs)
			if err != nil {
				t.Fatal(err)
			}
			assertClose(t, "value", score.Value, tt.wantValue, 0.001)
			if len(gen.prompts) != tt.wantCalls {
				t.Fatalf("Generate calls: got %d, want %d", len(gen.prompts), tt.wantCalls)
			}
			firstA := strings.Index(gen.prompts[0], "BASE-ANSWER") < strings.Index(gen.prompts[0], "EXP-ANSWER")
			if !firstA {
				t.Error("first call must show the base response as Response A")
			}
			if tt.wantCalls == 2 {
				swapped := strings.Index(gen.prompts[1], "EXP-ANSWER") < strings.Index(gen.prompts[1], "BASE-ANSWER")
				if !swapped {
					t.Error("second call must show the experimental response as Response A")
				}
			}
			if tt.wantStable == nil {
				if score.Samples != nil {
					t.Errorf("single call should not report samples, got %+v", score.Samples)
				}
				return
			}
			if score.Samples == nil || score.Samples.Stable != *tt.wantStable {
				t.Fatalf("Samples.Stable: got %+v, want %v", score.Samples, *tt.wantStable)
			}
			if !strings.Contains(score.Reason, "position-inconsistent") {
				t.Errorf("reason should flag position inconsistency, got %q", score.Reason)
			}
		})
	}
}

func TestPairwiseJudgeConsistentVerdictIsStable(t *testing.T) {
	// Prefers the experimental answer in both orders.
	g := generatorFunc(func(_ context.Context, prompt string) (string, error) {
		if strings.Index(prompt, "EXP") < strings.Index(prompt, "BASE") {
			return "SCORE: 0.1", nil // exp is A, judged better
		}
		return "SCORE: 0.9", nil // exp is B, judged better
	})
	obs := Observation{GroundTruth: json.RawMessage(`"BASE"`), Output: json.RawMessage(`"EXP"`)}
	score, err := NewPairwiseJudgeScorer(g).Score(context.Background(), obs)
	if err != nil {
		t.Fatal(err)
	}
	assertClose(t, "value", score.Value, 0.9, 0.001)
	if score.Samples == nil || !score.Samples.Stable {
		t.Errorf("consistent verdicts should be stable, got %+v", score.Samples)
	}
}

type generatorFunc func(ctx context.Context, prompt string) (string, error)

func (f generatorFunc) Generate(ctx context.Context, prompt string) (string, error) {
	return f(ctx, prompt)
}

func boolPtr(b bool) *bool { return &b }

func TestPairwiseJudgeSwapSurvivesSampling(t *testing.T) {
	consistent := generatorFunc(func(_ context.Context, prompt string) (string, error) {
		if strings.Index(prompt, "EXP-ANSWER") < strings.Index(prompt, "BASE-ANSWER") {
			return "SCORE: 0.1", nil
		}
		return "SCORE: 0.9", nil
	})
	obs := Observation{
		ID:          "pw",
		Input:       json.RawMessage(`"query"`),
		GroundTruth: json.RawMessage(`"BASE-ANSWER"`),
		Output:      json.RawMessage(`"EXP-ANSWER"`),
	}
	tests := []struct {
		name         string
		gen          Generator
		wantUnstable int
		wantReason   string
	}{
		{name: "position-inconsistent judge", gen: &positionBiasedGenerator{}, wantUnstable: 1, wantReason: "position-inconsistent"},
		{name: "consistent judge", gen: consistent, wantUnstable: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A wide tolerance means the sampled values alone never look
			// unstable; only the swap can flag the score.
			suite, err := Run(context.Background(), "swap", []Observation{obs},
				[]Scorer{NewPairwiseJudgeScorer(tt.gen)}, WithSampler(Sampler{N: 2, Tolerance: 1}))
			if err != nil {
				t.Fatal(err)
			}
			if suite.UnstableScores != tt.wantUnstable {
				t.Errorf("UnstableScores = %d, want %d", suite.UnstableScores, tt.wantUnstable)
			}
			score := suite.Results[0].Scores[0]
			if score.Samples == nil || len(score.Samples.Values) != 2 {
				t.Fatalf("expected 2 samples, got %+v", score.Samples)
			}
			if tt.wantReason != "" {
				found := false
				for _, r := range score.Samples.Reasons {
					found = found || strings.Contains(r, tt.wantReason)
				}
				if !found {
					t.Errorf("sample reasons %q should keep %q", score.Samples.Reasons, tt.wantReason)
				}
			}
		})
	}
}
