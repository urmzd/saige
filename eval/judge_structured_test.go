package eval

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestParseJudgeOutputStructured(t *testing.T) {
	tests := []struct {
		name       string
		reply      string
		wantScore  float64
		wantReason string
		wantErr    bool
	}{
		{"json object", `{"reasoning": "solid", "score": 0.75}`, 0.75, "solid", false},
		{"json in code fence", "```json\n{\"reasoning\": \"ok\", \"score\": 0.4}\n```", 0.4, "ok", false},
		{"json after prose", "Here is my verdict:\n{\"score\": 1, \"reasoning\": \"exact\"}", 1, "exact", false},
		{"json score as fraction string", `{"reasoning": "r", "score": "8/10"}`, 0.8, "r", false},
		{"json score clamped", `{"reasoning": "r", "score": 7}`, 1, "r", false},
		{"json reason key", `{"reason": "short", "score": 0.2}`, 0.2, "short", false},
		{"json without score falls back to lines", "{\"reasoning\": \"x\"}\nSCORE: 0.6", 0.6, "", false},
		{"brace in prose then json", "I use {braces} a lot. {\"score\": 0.3, \"reasoning\": \"b\"}", 0.3, "b", false},
		{"text fallback", "REASONING: legacy\nSCORE: 0.9", 0.9, "legacy", false},
		{"json score not a number", `{"reasoning": "r", "score": "high"}`, 0, "", true},
		{"nothing", "I refuse.", 0, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			score, reason, err := parseJudgeOutput(tt.reply)
			if tt.wantErr {
				if !errors.Is(err, ErrNoJudgeScore) {
					t.Fatalf("err = %v, want ErrNoJudgeScore", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			assertClose(t, "score", score, tt.wantScore, 1e-9)
			if reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}
}

type structuredGenerator struct {
	reply   string
	schemas []json.RawMessage
	plain   int
}

func (g *structuredGenerator) Generate(context.Context, string) (string, error) {
	g.plain++
	return "SCORE: 0", nil
}

func (g *structuredGenerator) GenerateStructured(_ context.Context, _ string, schema json.RawMessage) (string, error) {
	g.schemas = append(g.schemas, schema)
	return g.reply, nil
}

func TestJudgeUsesStructuredGenerator(t *testing.T) {
	tests := []struct {
		name      string
		scorer    func(Generator) Scorer
		wantCalls int
		want      float64
	}{
		{"pointwise", func(g Generator) Scorer { return NewJudgeScorer(g) }, 1, 0.7},
		// The swap mirrors the second verdict: (0.7 + (1 - 0.7)) / 2.
		{"pairwise", func(g Generator) Scorer { return NewPairwiseJudgeScorer(g) }, 2, 0.5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gen := &structuredGenerator{reply: `{"reasoning": "fine", "score": 0.7}`}
			score, err := tt.scorer(gen).Score(context.Background(), Observation{
				Input: json.RawMessage(`"q"`), Output: json.RawMessage(`"a"`), GroundTruth: json.RawMessage(`"b"`),
			})
			if err != nil {
				t.Fatal(err)
			}
			if gen.plain != 0 || len(gen.schemas) != tt.wantCalls {
				t.Fatalf("plain calls %d, structured calls %d; want 0 and %d", gen.plain, len(gen.schemas), tt.wantCalls)
			}
			if string(gen.schemas[0]) != string(JudgeSchema) {
				t.Fatalf("schema = %s, want JudgeSchema", gen.schemas[0])
			}
			assertClose(t, "score", score.Value, tt.want, 1e-9)
		})
	}
}

func TestJudgeSchemaIsValidAndMatchesPromptShape(t *testing.T) {
	schema, err := CompileJSONSchema(JudgeSchema)
	if err != nil {
		t.Fatal(err)
	}
	var verdict any
	_ = json.Unmarshal([]byte(`{"reasoning": "r", "score": 0.5}`), &verdict)
	if problems := schema.Validate(verdict); len(problems) != 0 {
		t.Fatalf("verdict rejected: %v", problems)
	}
	for _, prompt := range []string{judgePromptRaw, judgePairwisePromptRaw} {
		if !strings.Contains(prompt, `"reasoning"`) || !strings.Contains(prompt, `"score"`) {
			t.Fatalf("prompt does not ask for the JSON verdict:\n%s", prompt)
		}
	}
}
