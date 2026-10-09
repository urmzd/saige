package eval

import (
	"context"
	"encoding/json"

	topeval "github.com/urmzd/saige/eval"
)

// Annotation keys used by agent subjects.
const (
	AnnotationStreamTiming = "agent.stream_timing" // StreamTiming
	AnnotationToolCalls    = "agent.tool_calls"    // []ToolCallRecord
	AnnotationTurnCount    = "agent.turn_count"    // int
)

// ToolCallRecord captures a tool invocation for evaluation. [CollectAgentRun]
// builds these from a delta stream.
type ToolCallRecord struct {
	// ID is the provider's tool call ID, empty when the stream had none.
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
	// ArgumentsError is set when the call's argument text was not valid
	// JSON. Arguments is nil in that case, so this tells a malformed call
	// apart from one that took no arguments.
	ArgumentsError string `json:"arguments_error,omitempty"`
	Result         string `json:"result"`
	Error          string `json:"error,omitempty"`
	DurationMs     int64  `json:"duration_ms"`
	// Exec says how far execution got. [CollectAgentRun] always sets it;
	// it is empty on records built by hand, which are read as finished.
	Exec ExecState `json:"exec,omitempty"`
	// Version is the version the tool reported (types.ToolVersion), such as
	// an agent.Func schema hash or an agent.AIFunc version.
	Version string `json:"version,omitempty"`
}

// ExecState is how far a recorded tool call got through execution.
type ExecState string

const (
	// ExecNotRun marks a call the model announced that never started
	// executing.
	ExecNotRun ExecState = "not_run"
	// ExecUnfinished marks a call that started executing but never
	// reported an end, such as one that hung, timed out, or was canceled.
	// Its DurationMs is zero.
	ExecUnfinished ExecState = "unfinished"
	// ExecFinished marks a call whose execution ended, with a result or an
	// error.
	ExecFinished ExecState = "finished"
)

// TTFTScorer reports time-to-first-token in milliseconds.
func TTFTScorer() topeval.Scorer {
	return topeval.NewScorerFunc("ttft_ms", func(_ context.Context, obs topeval.Observation) (topeval.Score, error) {
		st, err := extractStreamTiming(obs)
		if err != nil || st == nil {
			return topeval.Score{}, err
		}
		return topeval.Score{Name: "ttft_ms", Value: float64(st.TTFTMs)}, nil
	})
}

// TTLTScorer reports time-to-last-token in milliseconds.
func TTLTScorer() topeval.Scorer {
	return topeval.NewScorerFunc("ttlt_ms", func(_ context.Context, obs topeval.Observation) (topeval.Score, error) {
		st, err := extractStreamTiming(obs)
		if err != nil || st == nil {
			return topeval.Score{}, err
		}
		return topeval.Score{Name: "ttlt_ms", Value: float64(st.TTLTMs)}, nil
	})
}

// MedianITLScorer reports median inter-token latency in milliseconds.
func MedianITLScorer() topeval.Scorer {
	return topeval.NewScorerFunc("median_itl_ms", func(_ context.Context, obs topeval.Observation) (topeval.Score, error) {
		st, err := extractStreamTiming(obs)
		if err != nil || st == nil {
			return topeval.Score{}, err
		}
		return topeval.Score{Name: "median_itl_ms", Value: st.MedianITL}, nil
	})
}

// ToolCallCountScorer reports the number of tool calls made.
func ToolCallCountScorer() topeval.Scorer {
	return topeval.NewScorerFunc("tool_call_count", func(_ context.Context, obs topeval.Observation) (topeval.Score, error) {
		calls, err := extractToolCalls(obs)
		if err != nil || calls == nil {
			return topeval.Score{}, err
		}
		return topeval.Score{Name: "tool_call_count", Value: float64(len(calls))}, nil
	})
}

// toolSuccessRateName is the score and registry name of ToolSuccessRateScorer.
const toolSuccessRateName = "tool_success_rate"

// ToolSuccessRateScorer reports the fraction of tool calls without errors.
// A call succeeds only when its execution finished ([ExecFinished], or an
// empty Exec on a hand-built record) with no error. A call whose arguments
// failed to parse counts as failed even when no execution error was
// recorded, and so does an [ExecUnfinished] call, which started but never
// ended. An [ExecNotRun] call with no error is left out of the rate: it was
// never executed, so it neither succeeded nor failed. With no counted calls
// the rate is 1.
func ToolSuccessRateScorer() topeval.Scorer {
	return topeval.NewScorerFunc(toolSuccessRateName, func(_ context.Context, obs topeval.Observation) (topeval.Score, error) {
		calls, err := extractToolCalls(obs)
		if err != nil || calls == nil {
			return topeval.Score{}, err
		}
		var success, counted int
		for _, c := range calls {
			failed := c.Error != "" || c.ArgumentsError != ""
			switch {
			case !failed && c.Exec == ExecNotRun:
				continue
			case !failed && (c.Exec == "" || c.Exec == ExecFinished):
				success++
			}
			counted++
		}
		if counted == 0 {
			return topeval.Score{Name: toolSuccessRateName, Value: 1.0}, nil
		}
		return topeval.Score{Name: toolSuccessRateName, Value: float64(success) / float64(counted)}, nil
	})
}

// TurnCountScorer reports the number of agent loop iterations.
func TurnCountScorer() topeval.Scorer {
	return topeval.NewScorerFunc("turn_count", func(_ context.Context, obs topeval.Observation) (topeval.Score, error) {
		raw, ok := obs.Annotations[AnnotationTurnCount]
		if !ok {
			return topeval.Score{}, nil
		}
		var count int
		if err := json.Unmarshal(raw, &count); err != nil {
			return topeval.Score{}, err
		}
		return topeval.Score{Name: "turn_count", Value: float64(count)}, nil
	})
}

func extractStreamTiming(obs topeval.Observation) (*StreamTiming, error) {
	raw, ok := obs.Annotations[AnnotationStreamTiming]
	if !ok {
		return nil, nil
	}
	var st StreamTiming
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func extractToolCalls(obs topeval.Observation) ([]ToolCallRecord, error) {
	raw, ok := obs.Annotations[AnnotationToolCalls]
	if !ok {
		return nil, nil
	}
	var calls []ToolCallRecord
	if err := json.Unmarshal(raw, &calls); err != nil {
		return nil, err
	}
	return calls, nil
}
