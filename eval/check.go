package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

// Check scorers are deterministic oracles over an observation's output: an
// output contract such as a JSON schema or a field value, or a text check
// such as a regular expression count. Each scores 1 when the check holds and
// 0 when it does not, with a reason naming what went wrong, and is marked
// [Deterministic] so sampling scores it once. A malformed output is a failed
// check, not a scorer error: the contract is what is being measured.

// ErrOutputNotJSON is the reason behind a failed JSON check when the output
// is neither a JSON document nor a JSON string holding one.
var ErrOutputNotJSON = errors.New("output is not JSON")

// OutputText returns the observation's output as text. A JSON string output
// is decoded; any other JSON value is returned as its compact JSON text; an
// empty output is "".
func OutputText(obs Observation) string {
	return rawText(obs.Output)
}

func rawText(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return trimmed
}

// OutputJSON decodes the observation's output as a JSON value. Agent and
// LLM subjects usually store the reply as a JSON string, so a string output
// is decoded again as JSON, after removing a surrounding markdown code
// fence. It returns an error wrapping [ErrOutputNotJSON] when no JSON value
// can be read.
func OutputJSON(obs Observation) (any, error) {
	return decodeJSONValue(obs.Output)
}

func decodeJSONValue(raw json.RawMessage) (any, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, fmt.Errorf("%w: empty", ErrOutputNotJSON)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrOutputNotJSON, err)
	}
	s, ok := v.(string)
	if !ok {
		return v, nil
	}
	inner := stripCodeFence(s)
	var nested any
	if err := json.Unmarshal([]byte(inner), &nested); err != nil {
		return nil, fmt.Errorf("%w: string %q does not hold a JSON document", ErrOutputNotJSON, excerpt(s, 80))
	}
	return nested, nil
}

// stripCodeFence removes a markdown code fence around s, such as
// "```json\n{...}\n```", and returns s trimmed otherwise.
func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") || !strings.HasSuffix(s, "```") || len(s) < 6 {
		return s
	}
	body := strings.TrimSuffix(strings.TrimPrefix(s, "```"), "```")
	if nl := strings.IndexByte(body, '\n'); nl >= 0 && !strings.ContainsAny(body[:nl], "{[\"") {
		body = body[nl+1:]
	}
	return strings.TrimSpace(body)
}

// NewCheckScorer builds a deterministic oracle scorer from a predicate. The
// check returns whether the observation passes and, when it does not, a
// reason. A returned error is recorded as an errored score, except
// [ErrNotApplicable], which declines an observation the check does not
// cover.
func NewCheckScorer(name string, check func(ctx context.Context, obs Observation) (bool, string, error)) Scorer {
	return Deterministic(NewScorerFunc(name, func(ctx context.Context, obs Observation) (Score, error) {
		ok, reason, err := check(ctx, obs)
		if errors.Is(err, ErrNotApplicable) {
			return Score{}, nil
		}
		if err != nil {
			return Score{}, err
		}
		return passFail(name, ok, reason), nil
	}))
}

// ErrNotApplicable lets a [NewCheckScorer] predicate decline an observation.
// The scorer then returns an empty [Score], which the framework skips.
var ErrNotApplicable = errors.New("check does not apply to this observation")

func passFail(name string, ok bool, reason string) Score {
	if ok {
		return Score{Name: name, Value: 1, Reason: reason}
	}
	return Score{Name: name, Value: 0, Reason: reason}
}

// JSONSchemaScorer checks the output against a JSON Schema (see
// [JSONSchema] for the supported keywords). The output is read with
// [OutputJSON]. The metric is "json_schema". The schema is compiled once;
// an invalid schema is returned as an error.
func JSONSchemaScorer(schema json.RawMessage) (Scorer, error) {
	compiled, err := CompileJSONSchema(schema)
	if err != nil {
		return nil, err
	}
	const name = "json_schema"
	return NewCheckScorer(name, func(_ context.Context, obs Observation) (bool, string, error) {
		v, err := OutputJSON(obs)
		if err != nil {
			return false, err.Error(), nil
		}
		problems := compiled.Validate(v)
		if len(problems) == 0 {
			return true, "", nil
		}
		return false, summarizeProblems(problems, 3), nil
	}), nil
}

func summarizeProblems(problems []string, limit int) string {
	if len(problems) <= limit {
		return strings.Join(problems, "; ")
	}
	return strings.Join(problems[:limit], "; ") + fmt.Sprintf("; and %d more", len(problems)-limit)
}

// JSONFieldScorer checks that the field at path in the output equals want.
// The output is read with [OutputJSON]. path is a dot-separated list of
// object keys and array indexes, such as "answer.city" or "items.0.id"; an
// empty path is the whole output. Values are compared by their JSON form, so
// 3 and 3.0 match. The metric is "json_field:" plus the path.
func JSONFieldScorer(path string, want any) Scorer {
	name := "json_field:" + path
	wantJSON := normalizeJSONValue(want)
	return NewCheckScorer(name, func(_ context.Context, obs Observation) (bool, string, error) {
		return checkField(obs, path, wantJSON)
	})
}

// JSONFieldGroundTruthScorer is [JSONFieldScorer] with the expected value
// read from the same path in the observation's ground truth, so one scorer
// serves a whole dataset. It declines observations with no ground truth and
// errors when the ground truth lacks the field. The metric is "json_field:"
// plus the path.
func JSONFieldGroundTruthScorer(path string) Scorer {
	name := "json_field:" + path
	return NewCheckScorer(name, func(_ context.Context, obs Observation) (bool, string, error) {
		if len(obs.GroundTruth) == 0 {
			return false, "", ErrNotApplicable
		}
		truth, err := decodeJSONValue(obs.GroundTruth)
		if err != nil {
			return false, "", fmt.Errorf("ground truth: %w", err)
		}
		want, ok, err := lookupPath(truth, path)
		if err != nil || !ok {
			return false, "", fmt.Errorf("ground truth has no field %q", path)
		}
		return checkField(obs, path, want)
	})
}

func checkField(obs Observation, path string, want any) (bool, string, error) {
	v, err := OutputJSON(obs)
	if err != nil {
		return false, err.Error(), nil
	}
	got, ok, err := lookupPath(v, path)
	if err != nil {
		return false, err.Error(), nil
	}
	if !ok {
		return false, fmt.Sprintf("field %q is missing", path), nil
	}
	if reflect.DeepEqual(got, want) {
		return true, "", nil
	}
	return false, fmt.Sprintf("field %q is %s, want %s", path, compactJSON(got), compactJSON(want)), nil
}

// lookupPath walks a decoded JSON value by a dot-separated path. ok is false
// when a key or index is absent; err is set when the path walks into a
// scalar.
func lookupPath(v any, path string) (value any, ok bool, err error) {
	if path == "" {
		return v, true, nil
	}
	cur := v
	walked := ""
	for _, part := range strings.Split(path, ".") {
		switch node := cur.(type) {
		case map[string]any:
			next, found := node[part]
			if !found {
				return nil, false, nil
			}
			cur = next
		case []any:
			i, convErr := strconv.Atoi(part)
			if convErr != nil {
				return nil, false, fmt.Errorf("field %q is an array; %q is not an index", walked, part)
			}
			if i < 0 || i >= len(node) {
				return nil, false, nil
			}
			cur = node[i]
		default:
			return nil, false, fmt.Errorf("field %q is %s, not an object or array", walked, jsonTypeName(cur))
		}
		if walked == "" {
			walked = part
		} else {
			walked += "." + part
		}
	}
	return cur, true, nil
}

// RegexCountScorer counts the non-overlapping matches of pattern in the
// output text ([OutputText]) and checks the count with op against n, so
// RegexCountScorer(`\[\d+\]`, GTE, 2) asks for at least two citations. The
// metric is "regex_count:" plus the pattern, op, and n, as in
// "regex_count:\[\d+\]>=2". An invalid pattern or op is returned as an
// error.
func RegexCountScorer(pattern string, op Op, n int) (Scorer, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("regex_count: %w", err)
	}
	if !op.valid() {
		return nil, fmt.Errorf("regex_count: unknown op %q", op)
	}
	name := fmt.Sprintf("regex_count:%s%s%d", pattern, op, n)
	return NewCheckScorer(name, func(_ context.Context, obs Observation) (bool, string, error) {
		count := len(re.FindAllStringIndex(OutputText(obs), -1))
		if op.Holds(float64(count), float64(n)) {
			return true, fmt.Sprintf("%d matches", count), nil
		}
		return false, fmt.Sprintf("%d matches, want %s %d", count, op, n), nil
	}), nil
}

// ContainsScorer checks that the output text ([OutputText]) contains every
// one of the substrings, case-sensitively. The metric is "contains:" plus
// the substrings joined by commas.
func ContainsScorer(substrings ...string) Scorer {
	name := "contains:" + strings.Join(substrings, ",")
	return NewCheckScorer(name, func(_ context.Context, obs Observation) (bool, string, error) {
		text := OutputText(obs)
		var missing []string
		for _, s := range substrings {
			if !strings.Contains(text, s) {
				missing = append(missing, strconv.Quote(s))
			}
		}
		if len(missing) == 0 {
			return true, "", nil
		}
		return false, "missing " + strings.Join(missing, ", "), nil
	})
}

// ExactMatchScorer checks that the output text equals the ground truth text
// after trimming surrounding whitespace. Both are read as with
// [OutputText]. It declines observations with no ground truth. The metric is
// "exact_match".
func ExactMatchScorer() Scorer {
	const name = "exact_match"
	return NewCheckScorer(name, func(_ context.Context, obs Observation) (bool, string, error) {
		if len(obs.GroundTruth) == 0 {
			return false, "", ErrNotApplicable
		}
		got := strings.TrimSpace(OutputText(obs))
		want := strings.TrimSpace(rawText(obs.GroundTruth))
		if got == want {
			return true, "", nil
		}
		return false, fmt.Sprintf("got %q, want %q", excerpt(got, 80), excerpt(want, 80)), nil
	})
}
