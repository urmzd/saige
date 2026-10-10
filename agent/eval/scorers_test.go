package eval

import (
	"context"
	"encoding/json"
	"math"
	"testing"

	"github.com/urmzd/saige/agent/types"
	topeval "github.com/urmzd/saige/eval"
)

func assertClose(t *testing.T, name string, got, want, eps float64) {
	t.Helper()
	if math.Abs(got-want) > eps {
		t.Errorf("%s: got %f, want %f (±%f)", name, got, want, eps)
	}
}

func TestTTFTScorer(t *testing.T) {
	st := StreamTiming{TTFTMs: 42}
	stJSON, _ := json.Marshal(st)

	obs := topeval.Observation{
		ID:          "t1",
		Annotations: map[string]json.RawMessage{AnnotationStreamTiming: stJSON},
	}

	score, err := TTFTScorer().Score(context.Background(), obs)
	if err != nil {
		t.Fatal(err)
	}
	assertClose(t, "ttft", score.Value, 42.0, 0.001)
}

func TestTTFTScorerMissingAnnotation(t *testing.T) {
	obs := topeval.Observation{ID: "t2"}
	score, err := TTFTScorer().Score(context.Background(), obs)
	if err != nil {
		t.Fatal(err)
	}
	if score.Name != "" {
		t.Errorf("expected empty score for missing annotation, got %q", score.Name)
	}
}

func TestToolSuccessRateScorer(t *testing.T) {
	calls := []ToolCallRecord{
		{Name: "search", Result: "ok"},
		{Name: "fetch", Error: "timeout"},
		{Name: "parse", Result: "done"},
	}
	callsJSON, _ := json.Marshal(calls)

	obs := topeval.Observation{
		ID:          "t3",
		Annotations: map[string]json.RawMessage{AnnotationToolCalls: callsJSON},
	}

	score, err := ToolSuccessRateScorer().Score(context.Background(), obs)
	if err != nil {
		t.Fatal(err)
	}
	// 2 out of 3 succeeded.
	assertClose(t, "success_rate", score.Value, 2.0/3.0, 0.001)
}

func TestToolCallCountScorer(t *testing.T) {
	calls := []ToolCallRecord{{Name: "a"}, {Name: "b"}}
	callsJSON, _ := json.Marshal(calls)

	obs := topeval.Observation{
		ID:          "t4",
		Annotations: map[string]json.RawMessage{AnnotationToolCalls: callsJSON},
	}

	score, err := ToolCallCountScorer().Score(context.Background(), obs)
	if err != nil {
		t.Fatal(err)
	}
	assertClose(t, "count", score.Value, 2.0, 0.001)
}

func TestTurnCountScorer(t *testing.T) {
	countJSON, _ := json.Marshal(5)
	obs := topeval.Observation{
		ID:          "t5",
		Annotations: map[string]json.RawMessage{AnnotationTurnCount: countJSON},
	}

	score, err := TurnCountScorer().Score(context.Background(), obs)
	if err != nil {
		t.Fatal(err)
	}
	assertClose(t, "turns", score.Value, 5.0, 0.001)
}

func TestToolSuccessRateScorerExecState(t *testing.T) {
	tests := []struct {
		name  string
		calls []ToolCallRecord
		want  float64
	}{
		{"finished calls succeed", []ToolCallRecord{{Name: "a", Exec: ExecFinished}, {Name: "b"}}, 1},
		{"unfinished call fails", []ToolCallRecord{{Name: "a", Exec: ExecFinished}, {Name: "b", Exec: ExecUnfinished}}, 0.5},
		{"call that never ran is left out", []ToolCallRecord{{Name: "a", Exec: ExecFinished}, {Name: "b", Exec: ExecNotRun}}, 1},
		{"refused call that never ran fails", []ToolCallRecord{{Name: "a", Exec: ExecFinished}, {Name: "b", Exec: ExecNotRun, ArgumentsError: "bad json"}}, 0.5},
		{"only calls that never ran", []ToolCallRecord{{Name: "a", Exec: ExecNotRun}}, 1},
		{"only an unfinished call", []ToolCallRecord{{Name: "a", Exec: ExecUnfinished}}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			callsJSON, _ := json.Marshal(tt.calls)
			obs := topeval.Observation{ID: "x", Annotations: map[string]json.RawMessage{AnnotationToolCalls: callsJSON}}
			score, err := ToolSuccessRateScorer().Score(context.Background(), obs)
			if err != nil {
				t.Fatal(err)
			}
			assertClose(t, tt.name, score.Value, tt.want, 0.001)
		})
	}
}

// TestToolSuccessRateScorerCollectedHang checks a collected run whose tool
// started and never ended: it is a failure, not a success.
func TestToolSuccessRateScorerCollectedHang(t *testing.T) {
	run := CollectAgentRun(feed(
		types.PartStart{Index: 0, Kind: types.KindToolCall, ID: "c1", Name: "slow"},
		types.PartEnd{Index: 0},
		types.ToolExecStartDelta{ToolCallID: "c1", Name: "slow"},
	))
	var obs topeval.Observation
	if err := AnnotateObservation(&obs, run); err != nil {
		t.Fatal(err)
	}
	score, err := ToolSuccessRateScorer().Score(context.Background(), obs)
	if err != nil {
		t.Fatal(err)
	}
	assertClose(t, "tool_success_rate", score.Value, 0, 0.001)
}
