package types

import (
	"context"
	"testing"
)

func TestEscalationLadder(t *testing.T) {
	ladder := EscalationLadder{Models: []string{"small", "medium", "large"}, Kinds: []OutcomeKind{OutcomeSchemaInvalid}}
	tests := []struct {
		name   string
		ladder EscalationLadder
		o      Outcome
		want   string // "" means no switch
	}{
		{"moves up one step", ladder, Outcome{Kind: OutcomeSchemaInvalid, Model: "small"}, "medium"},
		{"stops at the top", ladder, Outcome{Kind: OutcomeSchemaInvalid, Model: "large"}, ""},
		{"ignores other kinds", ladder, Outcome{Kind: OutcomeSubagentFailed, Model: "small"}, ""},
		{"unknown model starts at the bottom", ladder, Outcome{Kind: OutcomeSchemaInvalid, Model: "other"}, "small"},
		{"empty kinds match every outcome", EscalationLadder{Models: []string{"a", "b"}}, Outcome{Kind: OutcomeRefusal, Model: "a"}, "b"},
		{"empty ladder never switches", EscalationLadder{}, Outcome{Kind: OutcomeRefusal}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sw, err := tt.ladder.Observe(context.Background(), tt.o)
			if err != nil {
				t.Fatal(err)
			}
			got := ""
			if sw != nil {
				got = sw.Model
				if sw.Reason != string(tt.o.Kind) {
					t.Errorf("Reason = %q, want %q", sw.Reason, tt.o.Kind)
				}
			}
			if got != tt.want {
				t.Fatalf("switch = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOutcomePolicyFunc(t *testing.T) {
	var seen Outcome
	p := OutcomePolicyFunc(func(_ context.Context, o Outcome) (*Switch, error) {
		seen = o
		return &Switch{Model: "x"}, nil
	})
	sw, err := p.Observe(context.Background(), Outcome{Kind: OutcomeEvalScore, Score: 0.2})
	if err != nil || sw.Model != "x" || seen.Score != 0.2 {
		t.Fatalf("Observe = %+v, %v (saw %+v)", sw, err, seen)
	}
}
