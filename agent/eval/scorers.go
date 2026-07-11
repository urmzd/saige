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

// ToolCallRecord captures a tool invocation for evaluation.
type ToolCallRecord struct {
	Name       string         `json:"name"`
	Arguments  map[string]any `json:"arguments"`
	Result     string         `json:"result"`
	Error      string         `json:"error,omitempty"`
	DurationMs int64          `json:"duration_ms"`
}

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

// ToolSuccessRateScorer reports the fraction of tool calls without errors.
func ToolSuccessRateScorer() topeval.Scorer {
	return topeval.NewScorerFunc("tool_success_rate", func(_ context.Context, obs topeval.Observation) (topeval.Score, error) {
		calls, err := extractToolCalls(obs)
		if err != nil || calls == nil {
			return topeval.Score{}, err
		}
		if len(calls) == 0 {
			return topeval.Score{Name: "tool_success_rate", Value: 1.0}, nil
		}
		var success int
		for _, c := range calls {
			if c.Error == "" {
				success++
			}
		}
		return topeval.Score{Name: "tool_success_rate", Value: float64(success) / float64(len(calls))}, nil
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
