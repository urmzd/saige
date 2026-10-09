package harness

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
)

func TestChatWithRepair(t *testing.T) {
	validJSON := func(text string) error {
		var v map[string]any
		return json.Unmarshal([]byte(text), &v)
	}
	answer := func(text string) []types.Delta {
		return append(agenttest.TextResponse(text), types.UsageDelta{PromptTokens: 10, CompletionTokens: 2})
	}
	tests := []struct {
		name        string
		answers     []string
		maxRepairs  int
		validate    Validator
		wantValid   bool
		wantRepairs int
		wantCalls   int
		wantText    string
	}{
		{"valid first time", []string{`{"a":1}`}, 2, validJSON, true, 0, 1, `{"a":1}`},
		{"repaired once", []string{"nope", `{"a":1}`}, 2, validJSON, true, 1, 2, `{"a":1}`},
		{"repairs exhausted", []string{"nope", "still no", "never"}, 2, validJSON, false, 2, 3, "never"},
		{"no repairs allowed", []string{"nope"}, 0, validJSON, false, 0, 1, "nope"},
		{"nil validator accepts", []string{"anything"}, 2, nil, true, 0, 1, "anything"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var responses [][]types.Delta
			for _, a := range tt.answers {
				responses = append(responses, answer(a))
			}
			scripted := &agenttest.ScriptedProvider{Responses: responses}
			got, err := NewProviderClient(scripted).ChatWithRepair(context.Background(),
				[]Message{{Role: roleUser, Content: "give json"}}, tt.validate, RepairOptions{MaxRepairs: tt.maxRepairs})
			if err != nil {
				t.Fatal(err)
			}
			if got.Valid != tt.wantValid || got.Repairs != tt.wantRepairs || got.Result.Text != tt.wantText {
				t.Errorf("result = %+v", got)
			}
			if calls := scripted.CallCount(); calls != tt.wantCalls {
				t.Errorf("calls = %d, want %d", calls, tt.wantCalls)
			}
			if got.Result.InputTokens != uint64(10*tt.wantCalls) {
				t.Errorf("input tokens = %d, want summed over %d calls", got.Result.InputTokens, tt.wantCalls)
			}
			if !tt.wantValid && (got.Err == nil || len(got.Errors) != tt.wantCalls) {
				t.Errorf("errors = %v / %v", got.Err, got.Errors)
			}
			// Each repair call carries the rejected answer and a repair prompt.
			reqs := scripted.Requests()
			for i, req := range reqs {
				if want := 1 + 2*i; len(req.Messages) != want {
					t.Errorf("call %d sent %d messages, want %d", i, len(req.Messages), want)
				}
			}

			var turn TurnResult
			got.ApplyTo(&turn)
			if turn.RepairAttempts != tt.wantRepairs || turn.Failed == tt.wantValid {
				t.Errorf("turn = %+v", turn)
			}
			if !tt.wantValid && (turn.FailureReason == nil || turn.ValidationError == nil) {
				t.Errorf("failed turn lacks reasons: %+v", turn)
			}
		})
	}
}

func TestChatWithRepairStopsOnChatError(t *testing.T) {
	scripted := &agenttest.ScriptedProvider{
		Responses: [][]types.Delta{agenttest.TextResponse("bad")},
		Errors:    []error{nil, errors.New("down")},
	}
	got, err := NewProviderClient(scripted).ChatWithRepair(context.Background(),
		[]Message{{Role: roleUser, Content: "x"}}, func(string) error { return errors.New("invalid") }, RepairOptions{MaxRepairs: 3})
	if err == nil || got.Result.Text != "bad" {
		t.Fatalf("got %+v, %v; want the chat error with the first answer kept", got, err)
	}
}

func TestReliabilityCountsRepairFailure(t *testing.T) {
	var turn TurnResult
	RepairResult{Repairs: 1, Err: errors.New("schema mismatch")}.ApplyTo(&turn)
	rel := ComputeReliability([]TurnResult{turn})
	if rel == nil || rel.ValidationMissCount != 1 {
		t.Errorf("reliability = %+v", rel)
	}
}
