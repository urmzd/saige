package eval

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/urmzd/saige/agent/types"
	topeval "github.com/urmzd/saige/eval"
)

func init() {
	if err := RegisterScorers(topeval.DefaultRegistry); err != nil {
		panic(err)
	}
}

// RegisterScorers adds this package's scorers to r under their metric kinds,
// so suites can declare them as [topeval.ScorerSpec] values. Importing this
// package registers them in [topeval.DefaultRegistry]; call this for a
// private registry.
func RegisterScorers(r *topeval.Registry) error {
	type kind struct {
		name, description string
		factory           topeval.ScorerFactory
	}
	tools := func(build func(names ...string) topeval.Scorer) topeval.ScorerFactory {
		return func(params json.RawMessage) (topeval.Scorer, error) {
			var p struct {
				Tools []string `json:"tools"`
			}
			if err := topeval.DecodeParams(params, &p); err != nil {
				return nil, err
			}
			if len(p.Tools) == 0 {
				return nil, errors.New("params: tools is required")
			}
			return build(p.Tools...), nil
		}
	}
	kinds := []kind{
		{"ttft_ms", "time to first token in milliseconds", topeval.NoParams(TTFTScorer)},
		{"ttlt_ms", "time to last token in milliseconds", topeval.NoParams(TTLTScorer)},
		{"median_itl_ms", "median inter-token latency in milliseconds", topeval.NoParams(MedianITLScorer)},
		{"tool_call_count", "number of tool calls", topeval.NoParams(ToolCallCountScorer)},
		{toolSuccessRateName, "fraction of tool calls without errors", topeval.NoParams(ToolSuccessRateScorer)},
		{"turn_count", "number of agent loop iterations", topeval.NoParams(TurnCountScorer)},
		{"calls_in_order", `the tools were called in this order, as a subsequence; params: {"tools": [...]}`, tools(CallsInOrderScorer)},
		{"does_not_call", `none of the tools was called; params: {"tools": [...]}`, tools(DoesNotCallScorer)},
		{"only_calls", `every call was to one of the tools; params: {"tools": [...]}`, tools(OnlyCallsScorer)},
		{"calls_with", `some call to the tool had all the arguments; params: {"tool", "args": {...}}`,
			func(params json.RawMessage) (topeval.Scorer, error) {
				var p struct {
					Tool string         `json:"tool"`
					Args map[string]any `json:"args"`
				}
				if err := topeval.DecodeParams(params, &p); err != nil {
					return nil, err
				}
				if p.Tool == "" {
					return nil, errors.New("params: tool is required")
				}
				return CallsWithScorer(p.Tool, p.Args), nil
			}},
		{"tool_responds_within", `every call to the tool (all tools when omitted) executed within max_ms; params: {"tool", "max_ms"}`,
			func(params json.RawMessage) (topeval.Scorer, error) {
				var p struct {
					Tool  string `json:"tool"`
					MaxMs *int64 `json:"max_ms"`
				}
				if err := topeval.DecodeParams(params, &p); err != nil {
					return nil, err
				}
				if p.MaxMs == nil {
					return nil, errors.New("params: max_ms is required")
				}
				return ToolRespondsWithinScorer(p.Tool, time.Duration(*p.MaxMs)*time.Millisecond), nil
			}},
		{"cites", `the final answer cites its sources, each listed one when given; params: {"sources": [...]} (optional)`,
			func(params json.RawMessage) (topeval.Scorer, error) {
				var p struct {
					Sources []string `json:"sources"`
				}
				if err := topeval.DecodeParams(params, &p); err != nil {
					return nil, err
				}
				return CitesScorer(p.Sources...), nil
			}},
		{"refused", "the final turn is a refusal (holds a refusal part)", topeval.NoParams(RefusedScorer)},
		{"has_part", `the final turn holds a part of the kind; params: {"kind"}`,
			func(params json.RawMessage) (topeval.Scorer, error) {
				var p struct {
					Kind string `json:"kind"`
				}
				if err := topeval.DecodeParams(params, &p); err != nil {
					return nil, err
				}
				if p.Kind == "" {
					return nil, errors.New("params: kind is required")
				}
				return HasPartScorer(types.PartKind(p.Kind)), nil
			}},
		{"no_conversion", `no media was handled by the action (any conversion when omitted); params: {"action"}`,
			func(params json.RawMessage) (topeval.Scorer, error) {
				var p struct {
					Action string `json:"action"`
				}
				if err := topeval.DecodeParams(params, &p); err != nil {
					return nil, err
				}
				return NoConversionScorer(p.Action), nil
			}},
	}
	for _, k := range kinds {
		if err := r.Register(k.name, k.description, k.factory); err != nil {
			return err
		}
	}
	return nil
}
