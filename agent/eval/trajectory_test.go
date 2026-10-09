package eval

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	topeval "github.com/urmzd/saige/eval"
)

func trajectoryObs(t *testing.T, calls ...ToolCallRecord) topeval.Observation {
	t.Helper()
	raw, err := json.Marshal(calls)
	if err != nil {
		t.Fatal(err)
	}
	return topeval.Observation{ID: "traj", Annotations: map[string]json.RawMessage{AnnotationToolCalls: raw}}
}

func TestTrajectoryScorers(t *testing.T) {
	calls := []ToolCallRecord{
		{Name: "plan"},
		{Name: "search", Arguments: map[string]any{"q": "go", "limit": 5, "opts": map[string]any{"fuzzy": true}}},
		{Name: "fetch", Arguments: map[string]any{"url": "a"}},
		{Name: "answer"},
	}
	tests := []struct {
		name       string
		scorer     topeval.Scorer
		calls      []ToolCallRecord
		want       float64
		wantReason string
	}{
		{name: "in order subsequence", scorer: CallsInOrderScorer("search", "answer"), calls: calls, want: 1},
		{name: "in order wrong order", scorer: CallsInOrderScorer("answer", "search"), calls: calls, want: 0, wantReason: `"search" next`},
		{name: "in order missing tool", scorer: CallsInOrderScorer("search", "summarize"), calls: calls, want: 0, wantReason: `"summarize" next`},
		{name: "in order empty trajectory", scorer: CallsInOrderScorer("search"), calls: []ToolCallRecord{}, want: 0, wantReason: "no tools"},
		{name: "calls with match", scorer: CallsWithScorer("search", map[string]any{"q": "go", "limit": 5}), calls: calls, want: 1},
		{name: "calls with nested match", scorer: CallsWithScorer("search", map[string]any{"opts": map[string]any{"fuzzy": true}}), calls: calls, want: 1},
		{name: "calls with wrong value", scorer: CallsWithScorer("search", map[string]any{"q": "rust"}), calls: calls, want: 0, wantReason: "never with"},
		{name: "calls with never called", scorer: CallsWithScorer("delete", nil), calls: calls, want: 0, wantReason: "never called"},
		{name: "calls with malformed arguments", scorer: CallsWithScorer("search", map[string]any{"q": "go"}), calls: []ToolCallRecord{{Name: "search", ArgumentsError: "unexpected end of JSON input"}}, want: 0, wantReason: "1 had arguments that failed to parse"},
		{name: "does not call passes", scorer: DoesNotCallScorer("delete", "drop"), calls: calls, want: 1},
		{name: "does not call fails", scorer: DoesNotCallScorer("fetch"), calls: calls, want: 0, wantReason: "fetch"},
		{name: "only calls passes", scorer: OnlyCallsScorer("plan", "search", "fetch", "answer"), calls: calls, want: 1},
		{name: "only calls fails", scorer: OnlyCallsScorer("search", "answer"), calls: calls, want: 0, wantReason: "plan, fetch"},
		{name: "only calls empty trajectory", scorer: OnlyCallsScorer("search"), calls: []ToolCallRecord{}, want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			score, err := tt.scorer.Score(context.Background(), trajectoryObs(t, tt.calls...))
			if err != nil {
				t.Fatal(err)
			}
			assertClose(t, "value", score.Value, tt.want, 0)
			if tt.wantReason != "" && !strings.Contains(score.Reason, tt.wantReason) {
				t.Errorf("reason %q should contain %q", score.Reason, tt.wantReason)
			}
		})
	}
}

func TestTrajectoryScorersDeclineWithoutAnnotation(t *testing.T) {
	for _, sc := range []topeval.Scorer{
		CallsInOrderScorer("a"), CallsWithScorer("a", nil), DoesNotCallScorer("a"), OnlyCallsScorer("a"),
	} {
		score, err := sc.Score(context.Background(), topeval.Observation{ID: "none"})
		if err != nil || score.Name != "" {
			t.Errorf("%s: expected decline, got %+v, %v", sc.Name(), score, err)
		}
	}
}

func TestTrajectoryScorersAreDeterministic(t *testing.T) {
	sampled := topeval.Sampled(CallsInOrderScorer("a"), topeval.Sampler{N: 5})
	score, err := sampled.Score(context.Background(), trajectoryObs(t, ToolCallRecord{Name: "a"}))
	if err != nil {
		t.Fatal(err)
	}
	if score.Samples != nil {
		t.Error("deterministic scorer should not be resampled")
	}
}

func TestTrajectoryScorerNames(t *testing.T) {
	tests := []struct {
		scorer topeval.Scorer
		want   string
	}{
		{CallsInOrderScorer("search", "answer"), "calls_in_order:search,answer"},
		{CallsWithScorer("search", nil), "calls_with:search"},
		{CallsWithScorer("search", map[string]any{"q": "go", "k": 3}), `calls_with:search{k=3,q="go"}`},
		{DoesNotCallScorer("delete", "drop"), "does_not_call:delete,drop"},
		{OnlyCallsScorer("search"), "only_calls:search"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.scorer.Name(); got != tt.want {
				t.Errorf("Name() = %q, want %q", got, tt.want)
			}
			score, err := tt.scorer.Score(context.Background(), trajectoryObs(t, ToolCallRecord{Name: "search"}))
			if err != nil {
				t.Fatal(err)
			}
			if score.Name != tt.want {
				t.Errorf("Score.Name = %q, want %q", score.Name, tt.want)
			}
		})
	}
}

func TestTrajectoryScorersOfOneKindStaySeparate(t *testing.T) {
	obs := trajectoryObs(t, ToolCallRecord{Name: "search", Arguments: map[string]any{"q": "go"}})
	result, err := topeval.Run(context.Background(), "two-calls-with", []topeval.Observation{obs}, []topeval.Scorer{
		CallsWithScorer("search", nil),
		CallsWithScorer("fetch", nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Aggregate["calls_with:search"]; got != 1 {
		t.Errorf("calls_with:search = %v, want 1 (aggregate %v)", got, result.Aggregate)
	}
	if got, ok := result.Aggregate["calls_with:fetch"]; !ok || got != 0 {
		t.Errorf("calls_with:fetch = %v (present %v), want 0 (aggregate %v)", got, ok, result.Aggregate)
	}
}
