package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// TurnMetrics captures token usage, latency, and artifact size for one turn.
type TurnMetrics struct {
	InputTokens       uint64 `json:"input_tokens"`
	OutputTokens      uint64 `json:"output_tokens"`
	CachedInputTokens uint64 `json:"cached_input_tokens,omitempty"`
	LatencyMS         uint64 `json:"latency_ms"`
	ArtifactBytes     int    `json:"artifact_bytes"`
}

// TurnResult records one edit turn of a flow. Extra holds flow-specific
// fields that are flattened into the same JSON object on marshal (and
// recovered on unmarshal), so custom flows can add top-level keys such as
// envelope_parsed without changing the harness schema. Extra keys must not
// collide with the fixed field names (turn, edit, input_tokens,
// output_tokens, cached_input_tokens, latency_ms, output_bytes, retried,
// failed, failure_reason, inconclusive, repair_attempts, validation_error);
// MarshalJSON returns an error on collision.
//
// FailureReason follows the harness failure-reason contract used by
// [ComputeReliability]: reasons prefixed with "request failed",
// "envelope parse failed", "validation failed", "invalid envelope", or
// "apply failed" are classified into the matching Reliability counters. The
// built-in flows set "request failed: ..." when a chat request fails.
//
// Inconclusive marks a failed turn whose request failed on infrastructure
// (see [eval.IsInfra]), such as a rate limit or an outage: the model never
// answered, so the turn is recorded as unmeasured rather than as a failure.
type TurnResult struct {
	Turn              int
	Edit              string
	InputTokens       uint64
	OutputTokens      uint64
	CachedInputTokens uint64
	LatencyMS         uint64
	OutputBytes       int
	Retried           *bool
	Failed            bool
	FailureReason     *string
	Inconclusive      bool
	RepairAttempts    int
	ValidationError   *string
	Extra             map[string]any
}

// turnResultFixed mirrors TurnResult's fixed fields with the wire tags.
type turnResultFixed struct {
	Turn              int     `json:"turn"`
	Edit              string  `json:"edit"`
	InputTokens       uint64  `json:"input_tokens"`
	OutputTokens      uint64  `json:"output_tokens"`
	CachedInputTokens uint64  `json:"cached_input_tokens,omitempty"`
	LatencyMS         uint64  `json:"latency_ms"`
	OutputBytes       int     `json:"output_bytes"`
	Retried           *bool   `json:"retried,omitempty"`
	Failed            bool    `json:"failed"`
	FailureReason     *string `json:"failure_reason,omitempty"`
	Inconclusive      bool    `json:"inconclusive,omitempty"`
	RepairAttempts    int     `json:"repair_attempts,omitempty"`
	ValidationError   *string `json:"validation_error,omitempty"`
}

var turnResultFixedKeys = map[string]bool{
	"turn":                true,
	"edit":                true, //nolint:goconst // JSON field name; other occurrences are test values
	"input_tokens":        true,
	"output_tokens":       true,
	"cached_input_tokens": true,
	"latency_ms":          true,
	"output_bytes":        true,
	"retried":             true,
	"failed":              true,
	"failure_reason":      true,
	"inconclusive":        true,
	"repair_attempts":     true,
	"validation_error":    true,
}

// MarshalJSON flattens Extra into the same JSON object as the fixed fields.
func (t TurnResult) MarshalJSON() ([]byte, error) {
	fixed, err := json.Marshal(turnResultFixed{
		Turn:              t.Turn,
		Edit:              t.Edit,
		InputTokens:       t.InputTokens,
		OutputTokens:      t.OutputTokens,
		CachedInputTokens: t.CachedInputTokens,
		LatencyMS:         t.LatencyMS,
		OutputBytes:       t.OutputBytes,
		Retried:           t.Retried,
		Failed:            t.Failed,
		FailureReason:     t.FailureReason,
		Inconclusive:      t.Inconclusive,
		RepairAttempts:    t.RepairAttempts,
		ValidationError:   t.ValidationError,
	})
	if err != nil {
		return nil, err
	}
	return appendExtra(fixed, t.Extra, turnResultFixedKeys)
}

// UnmarshalJSON restores the fixed fields and collects unknown keys into
// Extra, inverting MarshalJSON.
func (t *TurnResult) UnmarshalJSON(data []byte) error {
	var fixed turnResultFixed
	if err := json.Unmarshal(data, &fixed); err != nil {
		return err
	}
	extra, err := splitExtra(data, turnResultFixedKeys)
	if err != nil {
		return err
	}
	*t = TurnResult{
		Turn:              fixed.Turn,
		Edit:              fixed.Edit,
		InputTokens:       fixed.InputTokens,
		OutputTokens:      fixed.OutputTokens,
		CachedInputTokens: fixed.CachedInputTokens,
		LatencyMS:         fixed.LatencyMS,
		OutputBytes:       fixed.OutputBytes,
		Retried:           fixed.Retried,
		Failed:            fixed.Failed,
		FailureReason:     fixed.FailureReason,
		Inconclusive:      fixed.Inconclusive,
		RepairAttempts:    fixed.RepairAttempts,
		ValidationError:   fixed.ValidationError,
		Extra:             extra,
	}
	return nil
}

// appendExtra splices extra keys (sorted for determinism) into a marshaled
// JSON object, rejecting keys that collide with reserved field names.
func appendExtra(fixed []byte, extra map[string]any, reserved map[string]bool) ([]byte, error) {
	if len(extra) == 0 {
		return fixed, nil
	}
	keys := make([]string, 0, len(extra))
	for key := range extra {
		if reserved[key] {
			return nil, fmt.Errorf("extra key %q collides with a fixed field", key)
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var buf bytes.Buffer
	buf.Write(fixed[:len(fixed)-1])
	for _, key := range keys {
		value, err := json.Marshal(extra[key])
		if err != nil {
			return nil, err
		}
		name, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		buf.WriteByte(',')
		buf.Write(name)
		buf.WriteByte(':')
		buf.Write(value)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// splitExtra returns the non-reserved keys of a JSON object decoded as any.
func splitExtra(data []byte, reserved map[string]bool) (map[string]any, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	var extra map[string]any
	for key, value := range raw {
		if reserved[key] {
			continue
		}
		var decoded any
		if err := json.Unmarshal(value, &decoded); err != nil {
			return nil, err
		}
		if extra == nil {
			extra = make(map[string]any)
		}
		extra[key] = decoded
	}
	return extra, nil
}

// FlowMetrics aggregates the per-turn results of one flow.
type FlowMetrics struct {
	PerTurn            []TurnResult `json:"per_turn"`
	TotalInputTokens   uint64       `json:"total_input_tokens"`
	TotalOutputTokens  uint64       `json:"total_output_tokens"`
	TotalCachedInput   uint64       `json:"total_cached_input_tokens,omitempty"`
	TotalLatencyMillis uint64       `json:"total_latency_ms"`
}

// ToFlowMetrics sums per-turn token and latency counters into a FlowMetrics.
func ToFlowMetrics(turns []TurnResult) FlowMetrics {
	var flow FlowMetrics
	flow.PerTurn = turns
	for _, turn := range turns {
		flow.TotalInputTokens += turn.InputTokens
		flow.TotalOutputTokens += turn.OutputTokens
		flow.TotalCachedInput += turn.CachedInputTokens
		flow.TotalLatencyMillis += turn.LatencyMS
	}
	return flow
}

// Comparison reports savings of one flow against a baseline flow, as
// percentages of the baseline totals.
//
// [CompareTurns] fills ComparedTurns and ExcludedTurns: savings then cover
// only the edit turns that succeeded in both flows, and ExcludedTurns counts
// the turns left out because either flow failed them.
type Comparison struct {
	OutputTokenSavingsPct float64 `json:"output_token_savings_pct"`
	InputTokenSavingsPct  float64 `json:"input_token_savings_pct"`
	LatencySavingsPct     float64 `json:"latency_savings_pct"`
	ComparedTurns         int     `json:"compared_turns,omitempty"`
	ExcludedTurns         int     `json:"excluded_turns,omitempty"`
}

// Compare computes token and latency savings of other relative to base.
func Compare(base, other FlowMetrics) Comparison {
	return Comparison{
		OutputTokenSavingsPct: Pct(base.TotalOutputTokens, other.TotalOutputTokens),
		InputTokenSavingsPct:  Pct(base.TotalInputTokens, other.TotalInputTokens),
		LatencySavingsPct:     Pct(base.TotalLatencyMillis, other.TotalLatencyMillis),
	}
}

// CompareTurns computes the savings of other relative to base over the edit
// turns that succeeded in both flows, matched by turn index. A failed turn
// spends latency and reports few or no tokens, so counting it would show
// savings the flow did not earn. Turns present in only one flow are also
// left out. When no turn is comparable every savings value is 0.
func CompareTurns(base, other []TurnResult) Comparison {
	failed := make(map[int]bool, len(base)+len(other))
	inBase := make(map[int]bool, len(base))
	inOther := make(map[int]bool, len(other))
	for _, t := range base {
		inBase[t.Turn] = true
		if t.Failed {
			failed[t.Turn] = true
		}
	}
	for _, t := range other {
		inOther[t.Turn] = true
		if t.Failed {
			failed[t.Turn] = true
		}
	}
	keep := func(turns []TurnResult, peer map[int]bool) []TurnResult {
		var kept []TurnResult
		for _, t := range turns {
			if !failed[t.Turn] && peer[t.Turn] {
				kept = append(kept, t)
			}
		}
		return kept
	}
	keptBase := keep(base, inOther)
	keptOther := keep(other, inBase)

	all := make(map[int]bool, len(inBase)+len(inOther))
	for turn := range inBase {
		all[turn] = true
	}
	for turn := range inOther {
		all[turn] = true
	}

	cmp := Compare(ToFlowMetrics(keptBase), ToFlowMetrics(keptOther))
	cmp.ComparedTurns = len(keptOther)
	cmp.ExcludedTurns = len(all) - len(keptOther)
	return cmp
}

// Reliability classifies failed edit turns by failure reason.
type Reliability struct {
	EditTurns            int            `json:"edit_turns"`
	MissCount            int            `json:"miss_count"`
	MissRate             float64        `json:"miss_rate"`
	ParseMissCount       int            `json:"parse_miss_count"`
	ValidationMissCount  int            `json:"validation_miss_count"`
	InvalidEnvelopeCount int            `json:"invalid_envelope_count"`
	ApplyMissCount       int            `json:"apply_miss_count"`
	RequestFailureCount  int            `json:"request_failure_count"`
	UnknownMissCount     int            `json:"unknown_miss_count"`
	ByReason             map[string]int `json:"by_reason,omitempty"`
}

// ComputeReliability classifies failed turns by the harness failure-reason
// prefix contract: "request failed", "envelope parse failed",
// "validation failed", "invalid envelope", and "apply failed". Turns whose Extra carries
// envelope_parsed=false count as request failures; turns with
// envelope_parsed=true but without apply_succeeded=true count as apply
// misses. Anything else falls into UnknownMissCount.
func ComputeReliability(turns []TurnResult) *Reliability {
	report := &Reliability{
		EditTurns: len(turns),
		ByReason:  map[string]int{},
	}
	for _, turn := range turns {
		if !turn.Failed {
			continue
		}
		report.MissCount++
		reason := ""
		if turn.FailureReason != nil {
			reason = *turn.FailureReason
			report.ByReason[reason]++
		}
		parsed, parsedSet := extraBool(turn.Extra, "envelope_parsed")
		applied, _ := extraBool(turn.Extra, "apply_succeeded")
		switch {
		case strings.HasPrefix(reason, requestFailedPrefix):
			report.RequestFailureCount++
		case strings.HasPrefix(reason, "envelope parse failed"):
			report.ParseMissCount++
		case strings.HasPrefix(reason, "validation failed"):
			report.ValidationMissCount++
		case strings.HasPrefix(reason, "invalid envelope"):
			report.InvalidEnvelopeCount++
		case strings.HasPrefix(reason, "apply failed"):
			report.ApplyMissCount++
		case parsedSet && !parsed:
			report.RequestFailureCount++
		case parsed && !applied:
			report.ApplyMissCount++
		default:
			report.UnknownMissCount++
		}
	}
	if report.MissCount == 0 {
		report.ByReason = nil
	}
	if report.EditTurns > 0 {
		report.MissRate = Round1(float64(report.MissCount)/float64(report.EditTurns)*100) / 100
	}
	return report
}

// extraBool reads a bool value from an Extra map; ok reports whether the
// key was present with a bool value.
func extraBool(extra map[string]any, key string) (value, ok bool) {
	raw, present := extra[key]
	if !present {
		return false, false
	}
	b, isBool := raw.(bool)
	if !isBool {
		return false, false
	}
	return b, true
}
