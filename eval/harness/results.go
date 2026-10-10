package harness

import (
	"context"
	"encoding/json"
	"math"
	"time"

	"github.com/urmzd/saige/eval"
	"github.com/urmzd/saige/eval/store"
)

// DefaultSuite is the suite name a [Runner] records its runs under when
// Runner.Suite is empty.
const DefaultSuite = "harness"

// Metric names of the scores [ScriptObservations] gives each turn.
const (
	MetricTurnSucceeded = "turn_succeeded"
	MetricLatencyMs     = "latency_ms"
	MetricInputTokens   = "input_tokens"
	MetricOutputTokens  = "output_tokens"
	MetricOutputBytes   = "output_bytes"
)

// ScriptObservations converts one script's flow results into scored
// observations: one per flow per turn, turn 0 included. Each observation has
// the script ID, the turn index, and the flow name as its
// [eval.LabelVariant], so its [eval.UnitKey] is unique within a run. Its
// timing holds the turn's latency and tokens, and its scores are
// turn_succeeded (1, or 0 with the failure reason, or an inconclusive error
// for a turn whose request failed on infrastructure), latency_ms,
// input_tokens, output_tokens, and output_bytes. flows gives the order;
// flows missing from results are skipped.
func ScriptObservations(script Script, flows []Flow, results map[string]FlowResult) []eval.ObservationResult {
	var out []eval.ObservationResult
	for _, flow := range flows {
		result, ok := results[flow.Name()]
		if !ok {
			continue
		}
		t0 := TurnResult{
			Turn:              0,
			InputTokens:       result.Turn0.InputTokens,
			OutputTokens:      result.Turn0.OutputTokens,
			CachedInputTokens: result.Turn0.CachedInputTokens,
			LatencyMS:         result.Turn0.LatencyMS,
			OutputBytes:       result.Turn0.ArtifactBytes,
		}
		if len(script.Turns) > 0 {
			t0.Edit = Truncate(script.Turns[0].Prompt, 80)
		}
		out = append(out, turnObservation(script, flow.Name(), t0))
		for _, turn := range result.Turns {
			out = append(out, turnObservation(script, flow.Name(), turn))
		}
	}
	return out
}

func turnObservation(script Script, flow string, turn TurnResult) eval.ObservationResult {
	input, _ := json.Marshal(turn.Edit)
	obs := eval.Observation{
		ID:     script.ID,
		Turn:   turn.Turn,
		Labels: eval.Labels{eval.LabelVariant: flow},
		Input:  input,
		Timing: eval.ObservationTiming{
			TotalMs:      int64(clampInt(turn.LatencyMS)),
			InputTokens:  clampInt(turn.InputTokens),
			OutputTokens: clampInt(turn.OutputTokens),
		},
	}
	if script.Format != "" {
		obs.Labels["format"] = script.Format
	}
	succeeded := eval.Score{Name: MetricTurnSucceeded, Value: 1}
	if turn.Failed {
		succeeded.Value = 0
		if turn.FailureReason != nil {
			succeeded.Reason = *turn.FailureReason
		}
	}
	if turn.Failed && turn.Inconclusive {
		// The model never answered: the turn is unmeasured, not failed.
		succeeded.Error, succeeded.Reason, succeeded.Inconclusive = succeeded.Reason, "", true
		if succeeded.Error == "" {
			succeeded.Error = "request failed on infrastructure"
		}
	}
	return eval.ObservationResult{
		Observation: obs,
		Scores: []eval.Score{
			succeeded,
			{Name: MetricLatencyMs, Value: float64(turn.LatencyMS)},
			{Name: MetricInputTokens, Value: float64(turn.InputTokens)},
			{Name: MetricOutputTokens, Value: float64(turn.OutputTokens)},
			{Name: MetricOutputBytes, Value: float64(turn.OutputBytes)},
		},
	}
}

// failedScriptObservation records a script whose flow failed as one
// unscored observation carrying the error, which [eval.SuiteResult] counts
// as a subject error, and as inconclusive when [eval.IsInfra] reports err.
func failedScriptObservation(script Script, flow string, err error) eval.ObservationResult {
	obs := eval.Observation{ID: script.ID, Labels: eval.Labels{}}
	eval.MarkSubjectError(&obs, err)
	if flow != "" {
		obs.Labels[eval.LabelVariant] = flow
	}
	return eval.ObservationResult{Observation: obs}
}

// suiteRecorder collects the observations of one [Runner.Run] for its
// results store.
type suiteRecorder struct {
	suite *eval.SuiteResult
}

func newSuiteRecorder(name string) *suiteRecorder {
	return &suiteRecorder{suite: &eval.SuiteResult{Name: name, CreatedAt: time.Now().UTC()}}
}

func (r *suiteRecorder) add(results ...eval.ObservationResult) {
	if r == nil {
		return
	}
	for _, res := range results {
		if eval.SubjectError(res.Observation) != "" {
			r.suite.SubjectErrors++
		}
		if res.Inconclusive() {
			r.suite.Inconclusive++
		}
		r.suite.Results = append(r.suite.Results, res)
	}
}

// save writes the collected suite as one run. runErr is the error the run
// returned; it sets the run's status.
func (r *suiteRecorder) save(ctx context.Context, s store.Store, id string, runErr error, prov eval.Provenance) (eval.RunRecord, error) {
	r.suite.Aggregate = eval.Aggregate(r.suite.Results)
	// The run is recorded even when ctx was canceled, so a partial run is
	// still visible in the store.
	return store.SaveSuite(context.WithoutCancel(ctx), s, id, r.suite, runErr, prov)
}

// clampInt converts a counter to int, saturating at math.MaxInt instead of
// wrapping negative.
func clampInt(v uint64) int {
	if v > math.MaxInt {
		return math.MaxInt
	}
	return int(v)
}
