package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	topeval "github.com/urmzd/saige/eval"
)

// Trajectory scorers check the tool calls recorded under
// [AnnotationToolCalls]. Each scores 1 when the check holds and 0 when it
// does not, with a reason naming what went wrong. They decline (return an
// empty Score) when the observation has no tool call annotation, and are
// marked [topeval.Deterministic] so sampling scores them once.
//
// Each metric name carries the scorer's arguments, for example
// "calls_with:search{q=\"go\"}" or "does_not_call:delete,drop", so two
// scorers of the same kind in one suite report separate metrics.

// CallsInOrderScorer checks that the named tools were called in this order.
// Other calls may come before, between, or after them: the names must appear
// as a subsequence of the trajectory. The metric is "calls_in_order:" plus
// the names joined by commas.
func CallsInOrderScorer(names ...string) topeval.Scorer {
	metric := "calls_in_order:" + strings.Join(names, ",")
	return trajectoryScorer(metric, func(calls []ToolCallRecord) (bool, string) {
		next := 0
		for _, c := range calls {
			if next < len(names) && c.Name == names[next] {
				next++
			}
		}
		if next == len(names) {
			return true, ""
		}
		return false, fmt.Sprintf("expected %q next (matched %d of %d in order); called %s",
			names[next], next, len(names), callNames(calls))
	})
}

// CallsWithScorer checks that at least one call to the named tool had all
// the given arguments. Arguments are compared by JSON value, so 3 and 3.0
// match, and arguments not listed in args are ignored. A call whose
// arguments failed to parse never matches, and the reason says how many such
// calls there were. The metric is "calls_with:" plus the tool name and, when
// args is not empty, the arguments, as in "calls_with:search{q=\"go\"}".
func CallsWithScorer(name string, args map[string]any) topeval.Scorer {
	metric := "calls_with:" + name
	if len(args) > 0 {
		metric += formatArgsCompact(args)
	}
	want, wantErr := normalizeJSON(args)
	return trajectoryScorer(metric, func(calls []ToolCallRecord) (bool, string) {
		if wantErr != nil {
			return false, fmt.Sprintf("expected arguments are not JSON: %v", wantErr)
		}
		wantArgs, _ := want.(map[string]any)
		seen, malformed := 0, 0
		for _, c := range calls {
			if c.Name != name {
				continue
			}
			seen++
			if c.ArgumentsError != "" {
				malformed++
				continue
			}
			got, err := normalizeJSON(c.Arguments)
			if err != nil {
				continue
			}
			gotArgs, _ := got.(map[string]any)
			if containsArgs(gotArgs, wantArgs) {
				return true, ""
			}
		}
		if seen == 0 {
			return false, fmt.Sprintf("%q was never called; called %s", name, callNames(calls))
		}
		reason := fmt.Sprintf("%q was called %d times, never with %s", name, seen, formatArgs(args))
		if malformed > 0 {
			reason += fmt.Sprintf(" (%d had arguments that failed to parse)", malformed)
		}
		return false, reason
	})
}

// DoesNotCallScorer checks that none of the named tools was called. The
// metric is "does_not_call:" plus the names joined by commas.
func DoesNotCallScorer(names ...string) topeval.Scorer {
	metric := "does_not_call:" + strings.Join(names, ",")
	forbidden := nameSet(names)
	return trajectoryScorer(metric, func(calls []ToolCallRecord) (bool, string) {
		var hits []string
		for _, c := range calls {
			if forbidden[c.Name] {
				hits = append(hits, c.Name)
			}
		}
		if len(hits) == 0 {
			return true, ""
		}
		return false, "called forbidden tools: " + strings.Join(hits, ", ")
	})
}

// OnlyCallsScorer checks that every call was to one of the named tools. A
// trajectory with no calls passes. The metric is "only_calls:" plus the
// names joined by commas.
func OnlyCallsScorer(names ...string) topeval.Scorer {
	metric := "only_calls:" + strings.Join(names, ",")
	allowed := nameSet(names)
	return trajectoryScorer(metric, func(calls []ToolCallRecord) (bool, string) {
		var extra []string
		for _, c := range calls {
			if !allowed[c.Name] {
				extra = append(extra, c.Name)
			}
		}
		if len(extra) == 0 {
			return true, ""
		}
		return false, "called tools outside the allowed set: " + strings.Join(extra, ", ")
	})
}

// ToolRespondsWithinScorer checks that every call to the named tool finished
// executing within limit, by the wall-clock DurationMs [CollectAgentRun]
// records. An empty name checks every call. A call that started executing
// but never finished ([ExecUnfinished]) counts as over the limit, since its
// duration is unknown. A call that never started ([ExecNotRun]) is left
// out. The scorer declines a trajectory with no executed matching calls, so
// a gate on it fails as a missing metric instead of passing on a tool that
// never ran. The metric is "tool_responds_within:" plus the name (or "*")
// and the limit, as in "tool_responds_within:search<=500ms".
func ToolRespondsWithinScorer(name string, limit time.Duration) topeval.Scorer {
	target := name
	if target == "" {
		target = "*"
	}
	metric := fmt.Sprintf("tool_responds_within:%s<=%dms", target, limit.Milliseconds())
	return topeval.Deterministic(topeval.NewScorerFunc(metric, func(_ context.Context, obs topeval.Observation) (topeval.Score, error) {
		calls, err := extractToolCalls(obs)
		if err != nil || calls == nil {
			return topeval.Score{}, err
		}
		matched := 0
		var slow []string
		var slowest int64
		for _, c := range calls {
			if name != "" && c.Name != name {
				continue
			}
			switch c.Exec {
			case ExecNotRun:
				continue
			case ExecUnfinished:
				matched++
				slow = append(slow, c.Name+" never finished")
				continue
			}
			matched++
			slowest = max(slowest, c.DurationMs)
			if time.Duration(c.DurationMs)*time.Millisecond > limit {
				slow = append(slow, fmt.Sprintf("%s took %dms", c.Name, c.DurationMs))
			}
		}
		if matched == 0 {
			return topeval.Score{}, nil
		}
		if len(slow) == 0 {
			return topeval.Score{Name: metric, Value: 1, Reason: fmt.Sprintf("%d calls, slowest %dms", matched, slowest)}, nil
		}
		return topeval.Score{Name: metric, Value: 0, Reason: fmt.Sprintf("over %s: %s", limit, strings.Join(slow, ", "))}, nil
	}))
}

func trajectoryScorer(metric string, check func([]ToolCallRecord) (bool, string)) topeval.Scorer {
	return topeval.Deterministic(topeval.NewScorerFunc(metric, func(_ context.Context, obs topeval.Observation) (topeval.Score, error) {
		calls, err := extractToolCalls(obs)
		if err != nil || calls == nil {
			return topeval.Score{}, err
		}
		ok, reason := check(calls)
		value := 0.0
		if ok {
			value = 1
		}
		return topeval.Score{Name: metric, Value: value, Reason: reason}, nil
	}))
}

func nameSet(names []string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return set
}

func callNames(calls []ToolCallRecord) string {
	if len(calls) == 0 {
		return "no tools"
	}
	names := make([]string, len(calls))
	for i, c := range calls {
		names[i] = c.Name
	}
	return "[" + strings.Join(names, ", ") + "]"
}

// normalizeJSON round-trips v through JSON so values compare the way they
// appear in a stored annotation (numbers as float64, nested maps as
// map[string]any).
func normalizeJSON(v any) (any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func containsArgs(got, want map[string]any) bool {
	for k, w := range want {
		g, ok := got[k]
		if !ok || !reflect.DeepEqual(g, w) {
			return false
		}
	}
	return true
}

// formatArgs renders args with sorted keys for reasons.
func formatArgs(args map[string]any) string {
	return "{" + strings.Join(argPairs(args), ", ") + "}"
}

// formatArgsCompact renders args with sorted keys and no spaces, for use in
// a metric name.
func formatArgsCompact(args map[string]any) string {
	return "{" + strings.Join(argPairs(args), ",") + "}"
}

func argPairs(args map[string]any) []string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		raw, _ := json.Marshal(args[k])
		parts[i] = k + "=" + string(raw)
	}
	return parts
}
