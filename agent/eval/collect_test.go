package eval

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/types"
	topeval "github.com/urmzd/saige/eval"
)

func feed(deltas ...any) <-chan types.Delta {
	ch := make(chan types.Delta, len(deltas))
	go func() {
		defer close(ch)
		for _, d := range deltas {
			switch v := d.(type) {
			case time.Duration:
				time.Sleep(v)
			case types.Delta:
				ch <- v
			}
		}
	}()
	return ch
}

func TestCollectAgentRunToolCalls(t *testing.T) {
	ch := feed(
		types.TextContentDelta{Content: "Let me look."},
		types.ToolCallStartDelta{ID: "c1", Name: "search"},
		types.ToolCallArgumentDelta{ID: "c1", Content: `{"q":"go"}`},
		types.ToolCallEndDelta{ID: "c1", Arguments: map[string]any{"q": "go"}},
		types.ToolCallStartDelta{ID: "c2", Name: "fetch"},
		types.ToolCallEndDelta{ID: "c2", Arguments: map[string]any{"url": "x"}},
		types.UsageDelta{PromptTokens: 10, CompletionTokens: 4},
		types.ToolExecStartDelta{ToolCallID: "c1", Name: "search"},
		types.ToolExecStartDelta{ToolCallID: "c2", Name: "fetch"},
		// Nested sub-agent activity must not enter the top-level trajectory.
		types.ToolExecDelta{ToolCallID: "c1", Inner: types.ToolCallStartDelta{ID: "inner", Name: "nested"}},
		types.ToolExecDelta{ToolCallID: "c1", Inner: types.TextContentDelta{Content: "inner text"}},
		3*time.Millisecond,
		types.ToolExecEndDelta{ToolCallID: "c2", Name: "fetch", Error: "timeout"},
		types.ToolExecEndDelta{ToolCallID: "c1", Name: "search", Result: "found"},
		types.TextContentDelta{Content: " Done."},
		types.UsageDelta{PromptTokens: 20, CompletionTokens: 6},
		types.DoneDelta{},
	)
	run := CollectAgentRun(ch)

	if run.Text != "Let me look. Done." {
		t.Errorf("text: got %q", run.Text)
	}
	if run.TurnCount != 2 {
		t.Errorf("TurnCount: got %d, want 2", run.TurnCount)
	}
	if run.Timing.InputTokens != 30 || run.Timing.OutputTokens != 10 {
		t.Errorf("tokens: got %d/%d", run.Timing.InputTokens, run.Timing.OutputTokens)
	}
	if len(run.ToolCalls) != 2 {
		t.Fatalf("tool calls: got %+v", run.ToolCalls)
	}
	tests := []struct {
		id, name, result, err string
		arg, val              string
	}{
		{id: "c1", name: "search", result: "found", arg: "q", val: "go"},
		{id: "c2", name: "fetch", err: "timeout", arg: "url", val: "x"},
	}
	for i, tt := range tests {
		got := run.ToolCalls[i]
		if got.ID != tt.id || got.Name != tt.name || got.Result != tt.result || got.Error != tt.err {
			t.Errorf("call %d: got %+v", i, got)
		}
		if got.Arguments[tt.arg] != tt.val {
			t.Errorf("call %d: arguments %v", i, got.Arguments)
		}
		if got.DurationMs <= 0 {
			t.Errorf("call %d: DurationMs %d, want > 0", i, got.DurationMs)
		}
	}
}

func TestCollectAgentRunPairsEndWithoutID(t *testing.T) {
	// Interleaved starts with ID-less ends: each end closes the oldest open call.
	run := CollectAgentRun(feed(
		types.ToolCallStartDelta{ID: "a", Name: "first"},
		types.ToolCallStartDelta{ID: "b", Name: "second"},
		types.ToolCallEndDelta{Arguments: map[string]any{"n": 1}},
		types.ToolCallEndDelta{Arguments: map[string]any{"n": 2}},
	))
	if len(run.ToolCalls) != 2 {
		t.Fatalf("got %+v", run.ToolCalls)
	}
	if run.ToolCalls[0].Arguments["n"] != 1 || run.ToolCalls[1].Arguments["n"] != 2 {
		t.Errorf("arguments paired wrongly: %+v", run.ToolCalls)
	}
}

func TestCollectAgentRunArgumentsError(t *testing.T) {
	run := CollectAgentRun(feed(
		types.ToolCallStartDelta{ID: "a", Name: "search"},
		types.ToolCallEndDelta{ID: "a", ArgumentsError: "unexpected end of JSON input"},
		types.ToolCallStartDelta{ID: "b", Name: "list"},
		types.ToolCallEndDelta{ID: "b"},
	))
	if len(run.ToolCalls) != 2 {
		t.Fatalf("got %+v", run.ToolCalls)
	}
	tests := []struct {
		name    string
		rec     ToolCallRecord
		wantErr string
	}{
		{name: "malformed", rec: run.ToolCalls[0], wantErr: "unexpected end of JSON input"},
		{name: "no arguments", rec: run.ToolCalls[1], wantErr: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.rec.Arguments != nil || tt.rec.ArgumentsError != tt.wantErr {
				t.Errorf("record = %+v, want ArgumentsError %q", tt.rec, tt.wantErr)
			}
		})
	}

	var obs topeval.Observation
	if err := AnnotateObservation(&obs, run); err != nil {
		t.Fatal(err)
	}
	score, err := ToolSuccessRateScorer().Score(context.Background(), obs)
	if err != nil {
		t.Fatal(err)
	}
	// The malformed call failed; the other never ran and is left out.
	assertClose(t, "tool_success_rate", score.Value, 0, 0.001)
}

func TestCollectAgentRunExecWithoutAnnouncedCall(t *testing.T) {
	run := CollectAgentRun(feed(
		types.ToolExecStartDelta{ToolCallID: "r1", Name: "replayed"},
		types.ToolExecEndDelta{ToolCallID: "r1", Result: "ok"},
	))
	if len(run.ToolCalls) != 1 || run.ToolCalls[0].Name != "replayed" || run.ToolCalls[0].Result != "ok" {
		t.Errorf("got %+v", run.ToolCalls)
	}
}

func TestAnnotateObservationFeedsScorers(t *testing.T) {
	run := CollectAgentRun(feed(
		types.ToolCallStartDelta{ID: "c1", Name: "search"},
		types.ToolCallEndDelta{ID: "c1", Arguments: map[string]any{"q": "go", "k": 3}},
		types.ToolExecStartDelta{ToolCallID: "c1", Name: "search"},
		types.ToolExecEndDelta{ToolCallID: "c1", Name: "search", Result: "r"},
		types.ToolCallStartDelta{ID: "c2", Name: "answer"},
		types.ToolCallEndDelta{ID: "c2"},
		types.ToolExecStartDelta{ToolCallID: "c2", Name: "answer"},
		types.ToolExecEndDelta{ToolCallID: "c2", Name: "answer", Error: "bad"},
		types.TextContentDelta{Content: "final"},
		types.UsageDelta{PromptTokens: 1, CompletionTokens: 1},
	))
	var obs topeval.Observation
	if err := AnnotateObservation(&obs, run); err != nil {
		t.Fatal(err)
	}
	if string(obs.Output) != `"final"` {
		t.Errorf("output: got %s", obs.Output)
	}

	tests := []struct {
		scorer topeval.Scorer
		want   float64
	}{
		{ToolCallCountScorer(), 2},
		{ToolSuccessRateScorer(), 0.5},
		{TurnCountScorer(), 1},
		{CallsInOrderScorer("search", "answer"), 1},
		{CallsWithScorer("search", map[string]any{"k": 3.0}), 1},
	}
	for _, tt := range tests {
		t.Run(tt.scorer.Name(), func(t *testing.T) {
			score, err := tt.scorer.Score(context.Background(), obs)
			if err != nil {
				t.Fatal(err)
			}
			assertClose(t, tt.scorer.Name(), score.Value, tt.want, 0.001)
		})
	}
	if _, ok := obs.Annotations[AnnotationStreamTiming]; !ok {
		t.Error("stream timing annotation missing")
	}
}

func TestAnnotateObservationKeepsOutput(t *testing.T) {
	obs := topeval.Observation{Output: json.RawMessage(`{"custom":true}`)}
	if err := AnnotateObservation(&obs, AgentRun{Text: "ignored"}); err != nil {
		t.Fatal(err)
	}
	if string(obs.Output) != `{"custom":true}` {
		t.Errorf("existing output overwritten: %s", obs.Output)
	}
	if string(obs.Annotations[AnnotationToolCalls]) != "[]" {
		t.Errorf("empty trajectory should annotate as [], got %s", obs.Annotations[AnnotationToolCalls])
	}
}

func TestCollectAgentRunRecordsErrors(t *testing.T) {
	run := CollectAgentRun(feed(
		types.TextContentDelta{Content: "partial"},
		types.ErrorDelta{Error: errors.New("provider overloaded")},
	))
	if !run.Timing.Failed() || run.Timing.Errors[0] != "provider overloaded" {
		t.Errorf("errors: got %v", run.Timing.Errors)
	}
}
