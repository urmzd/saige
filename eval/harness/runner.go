package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urmzd/saige/eval"
	"github.com/urmzd/saige/eval/store"
)

// Runner executes flows over scripts and writes one metrics document per
// script into <Script.Dir>/<MetricsFile>.
//
// Flows run in order and share one [FlowContext] per script, so later
// flows can seed from earlier ones. When Assemble is nil the runner writes a
// [DefaultMetrics] document; set Assemble to produce a custom schema from
// the collected flow results.
type Runner struct {
	Client      *Client
	Flows       []Flow
	Force       bool
	MetricsFile string // default "metrics.json"
	Assemble    func(script Script, results map[string]FlowResult) (any, error)
	// ContinueOnError keeps running the remaining scripts when one
	// fails. The failure is written to <Script.Dir>/error.json and all
	// failures are returned together at the end. Since no metrics file is
	// written for a failed script, the next run retries it.
	ContinueOnError bool
	// Concurrency is how many scripts run at once. Zero or one runs them
	// one at a time. Results are recorded in script order whatever the
	// concurrency, so stored runs and gates do not depend on timing. The
	// Client and the flows must be safe for concurrent use; the built-in
	// ones are. Without ContinueOnError a failure stops new scripts from
	// starting, and scripts already running finish.
	Concurrency int

	// Results, when set, records each Run as one run in a results store
	// through [store.SaveSuite], after the scripts finish (or fail). Every
	// executed script adds one unit per flow per turn (see
	// [ScriptObservations]); a failed script adds one unit carrying its
	// error. Skipped scripts add nothing.
	Results store.Store
	// RunID names the stored run; empty means [eval.NewRunID]. A Runner
	// that calls Run more than once should leave it empty, since run IDs
	// must be unique.
	RunID string
	// Suite is the stored run's suite name; empty means [DefaultSuite].
	Suite string
	// Provenance is recorded on the stored run. Client.Model is added to
	// its models.
	Provenance eval.Provenance
	// OnSaved, when set, is called with the stored run record.
	OnSaved func(eval.RunRecord)

	// Resume names an earlier run in Results to continue. Scripts that
	// completed in that run (every flow recorded, no script failure) are not
	// run again: their stored units are copied into this run, which is
	// saved under a new ID, so the new run is complete on its own. The
	// other scripts run even when their metrics file exists, as if Force
	// were set for them. Requires Results.
	Resume string

	// ReuseMetrics makes a script skipped because its metrics file exists
	// contribute units rebuilt from that file, so a rerun that skips
	// finished scripts still records and gates the whole corpus. It reads
	// the [DefaultMetrics] document and so applies only when Assemble is
	// nil. A file that does not decode, or that records a different model
	// than Client.Model, contributes nothing, so results of another model
	// are never counted as this run's.
	ReuseMetrics bool

	// Assert gates the run with [eval.SuiteResult.Gate] over the recorded
	// units, the same units Results would store. When a gate fails, Run
	// returns an [*AssertionError] (joined with any run error), and with
	// Results set the stored run carries the outcome and violations.
	// Skipped scripts add no units unless ReuseMetrics is set, so gate a
	// full run (Force, Resume or ReuseMetrics) rather than one that skipped
	// finished scripts.
	Assert []eval.Assertion
	// OnGated, when set, is called with the gated suite after Assert ran.
	OnGated func(*eval.SuiteResult)
}

// ErrorFile is the per-script failure record the [Runner] writes.
const ErrorFile = "error.json"

// ExperimentError is the content of a script's error.json.
type ExperimentError struct {
	ExperimentID string `json:"experiment_id"`
	Flow         string `json:"flow,omitempty"`
	Error        string `json:"error"`
	Timestamp    string `json:"timestamp"`
}

// ErrAssertionsFailed matches an [*AssertionError] with errors.Is.
var ErrAssertionsFailed = errors.New("eval assertions failed")

// AssertionError reports the violated gates of a run.
type AssertionError struct {
	Violations []eval.Violation
}

func (e *AssertionError) Error() string {
	lines := make([]string, len(e.Violations))
	for i, v := range e.Violations {
		lines[i] = v.String()
	}
	return fmt.Sprintf("%d assertion violation(s): %s", len(e.Violations), strings.Join(lines, "; "))
}

// Is matches [ErrAssertionsFailed].
func (e *AssertionError) Is(target error) bool { return target == ErrAssertionsFailed }

// Run executes all flows for each script. Scripts whose metrics file
// already exists are skipped unless Force is set. Progress is logged to
// stderr. A failed script writes error.json in its directory. Without
// ContinueOnError the first failure aborts the run; with it, every failure
// is joined into the returned error after all scripts ran.
//
// With Results set, the run is saved to the store after the scripts finish;
// a failure to save is joined into the returned error. With Assert set, the
// recorded units are gated and a failed gate is joined as an
// [*AssertionError].
func (r *Runner) Run(ctx context.Context, scripts []Script) error {
	if r.Resume != "" && r.Results == nil {
		return errNoResumeStore(r.Resume)
	}
	if r.Results == nil && len(r.Assert) == 0 {
		return r.run(ctx, scripts, nil, nil)
	}
	suite := r.Suite
	if suite == "" {
		suite = DefaultSuite
	}
	var carried map[string][]eval.ObservationResult
	if r.Resume != "" {
		var err error
		if carried, err = r.completedScripts(ctx); err != nil {
			return err
		}
	}
	rec := newSuiteRecorder(suite)
	runErr := r.run(ctx, scripts, rec, carried)

	var gateErr error
	if len(r.Assert) > 0 {
		rec.suite.Aggregate = eval.Aggregate(rec.suite.Results)
		if rec.suite.Gate(r.Assert...) == eval.OutcomeFailed {
			gateErr = &AssertionError{Violations: append([]eval.Violation(nil), rec.suite.Violations...)}
		}
		if r.OnGated != nil {
			r.OnGated(rec.suite)
		}
	}
	if r.Results == nil {
		return errors.Join(runErr, gateErr)
	}

	id := r.RunID
	if id == "" {
		id = eval.NewRunID()
	}
	prov := r.Provenance
	prov.Models = append([]string(nil), prov.Models...)
	if r.Client != nil {
		prov.AddModels(r.Client.Model)
	}
	run, err := rec.save(ctx, r.Results, id, runErr, prov)
	if err != nil {
		return errors.Join(runErr, gateErr, fmt.Errorf("save run %s: %w", id, err))
	}
	fmt.Fprintf(os.Stderr, "recorded run %s (%d units)\n", run.ID, run.Units)
	if r.OnSaved != nil {
		r.OnSaved(run)
	}
	return errors.Join(runErr, gateErr)
}

func errNoResumeStore(id string) error {
	return fmt.Errorf("resume %s: no results store to read it from", id)
}

// completedScripts loads the run named by Resume and returns, by script ID,
// the stored results of every script that completed in it: each of the
// runner's flows recorded at least one unit and no unit records a script
// failure. It refuses a run that did not call the runner's model, since
// carrying its results would record them under the wrong model.
func (r *Runner) completedScripts(ctx context.Context) (map[string][]eval.ObservationResult, error) {
	record, err := r.Results.GetRun(ctx, r.Resume)
	if err != nil {
		return nil, fmt.Errorf("resume %s: %w", r.Resume, err)
	}
	if r.Client != nil && !slices.Contains(record.Provenance.Models, r.Client.Model) {
		return nil, fmt.Errorf("resume %s: it ran model(s) %v, this run uses %q; resume with the same model or start a new run", r.Resume, record.Provenance.Models, r.Client.Model)
	}
	prior, err := store.LoadSuite(ctx, r.Results, r.Resume)
	if err != nil {
		return nil, fmt.Errorf("resume %s: %w", r.Resume, err)
	}
	byScript := map[string][]eval.ObservationResult{}
	failed := map[string]bool{}
	flows := map[string]map[string]bool{}
	for _, res := range prior.Results {
		id := res.Observation.ID
		byScript[id] = append(byScript[id], res)
		if eval.SubjectError(res.Observation) != "" {
			failed[id] = true
		}
		if flows[id] == nil {
			flows[id] = map[string]bool{}
		}
		flows[id][res.Observation.Labels[eval.LabelVariant]] = true
	}
	for id := range byScript {
		complete := !failed[id]
		for _, flow := range r.Flows {
			complete = complete && flows[id][flow.Name()]
		}
		if !complete {
			delete(byScript, id)
		}
	}
	return byScript, nil
}

// reusedObservations rebuilds a skipped script's units from its metrics
// file when ReuseMetrics applies, or returns nil.
func (r *Runner) reusedObservations(script Script, metricsPath string) []eval.ObservationResult {
	if !r.ReuseMetrics || r.Assemble != nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Clean(metricsPath))
	if err != nil {
		return nil
	}
	var doc DefaultMetrics
	if err := json.Unmarshal(data, &doc); err != nil {
		fmt.Fprintf(os.Stderr, "  %s: cannot reuse %s: %v\n", script.ID, metricsPath, err)
		return nil
	}
	if r.Client != nil && doc.Model != r.Client.Model {
		fmt.Fprintf(os.Stderr, "  %s: not reusing %s: it records model %q, this run uses %q\n", script.ID, metricsPath, doc.Model, r.Client.Model)
		return nil
	}
	for _, flow := range r.Flows {
		if _, ok := doc.Flows[flow.Name()]; !ok {
			fmt.Fprintf(os.Stderr, "  %s: not reusing %s: it has no %s flow; use --force to rerun\n", script.ID, metricsPath, flow.Name())
			return nil
		}
	}
	results := make(map[string]FlowResult, len(doc.Flows))
	for name, report := range doc.Flows {
		results[name] = FlowResult{Turn0: report.Turn0, Turns: report.PerTurn}
	}
	return ScriptObservations(script, r.Flows, results)
}

// scriptOutcome is what one script contributed to a run.
type scriptOutcome struct {
	started bool
	skipped bool
	carried []eval.ObservationResult
	flow    string
	results map[string]FlowResult
	err     error
}

func (r *Runner) run(ctx context.Context, scripts []Script, rec *suiteRecorder, carried map[string][]eval.ObservationResult) error {
	metricsFile := r.MetricsFile
	if metricsFile == "" {
		metricsFile = "metrics.json"
	}
	workers := max(r.Concurrency, 1)
	sem := make(chan struct{}, workers)
	outcomes := make([]scriptOutcome, len(scripts))
	var stop atomic.Bool
	var wg sync.WaitGroup

launch:
	for i, script := range scripts {
		if ctx.Err() != nil || stop.Load() {
			break
		}
		if prior, ok := carried[script.ID]; ok {
			fmt.Fprintf(os.Stderr, "[%d/%d] resume %s (%d units carried from run %s)\n", i+1, len(scripts), script.ID, len(prior), r.Resume)
			outcomes[i] = scriptOutcome{carried: prior}
			continue
		}
		metricsPath := filepath.Join(script.Dir, metricsFile)
		if !r.Force && r.Resume == "" {
			if _, err := os.Stat(metricsPath); err == nil {
				fmt.Fprintf(os.Stderr, "[%d/%d] skip %s (%s exists; use --force)\n", i+1, len(scripts), script.ID, metricsFile)
				outcomes[i] = scriptOutcome{skipped: true, carried: r.reusedObservations(script, metricsPath)}
				continue
			}
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break launch
		}
		if stop.Load() {
			<-sem
			break
		}
		fmt.Fprintf(os.Stderr, "[%d/%d] running %s\n", i+1, len(scripts), script.ID)
		wg.Add(1)
		go func(i int, script Script, metricsPath string) {
			defer wg.Done()
			defer func() { <-sem }()
			out := scriptOutcome{started: true}
			out.flow, out.results, out.err = r.runScript(ctx, script, metricsPath)
			if out.err == nil {
				_ = os.Remove(filepath.Join(script.Dir, ErrorFile))
				fmt.Fprintf(os.Stderr, "  wrote %s\n", metricsPath)
			} else {
				out.err = fmt.Errorf("%s: %w", script.ID, out.err)
				fmt.Fprintf(os.Stderr, "  failed: %v\n", out.err)
				if werr := writeExperimentError(script, out.flow, out.err); werr != nil {
					out.err = errors.Join(out.err, werr)
				}
				if !r.ContinueOnError || ctx.Err() != nil {
					stop.Store(true)
				}
			}
			outcomes[i] = out
		}(i, script, metricsPath)
	}
	wg.Wait()

	var failures []error
	for i, out := range outcomes {
		switch {
		case out.carried != nil:
			rec.add(out.carried...)
		case !out.started:
		case out.err != nil:
			rec.add(failedScriptObservation(scripts[i], out.flow, out.err))
			failures = append(failures, out.err)
		default:
			rec.add(ScriptObservations(scripts[i], r.Flows, out.results)...)
		}
	}
	if err := ctx.Err(); err != nil && !containsErr(failures, err) {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

// containsErr reports whether any of errs matches target.
func containsErr(errs []error, target error) bool {
	for _, err := range errs {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// runScript runs every flow over one script and writes its metrics
// document, returning the flow results by flow name. On failure it returns
// the name of the flow that failed, if any.
func (r *Runner) runScript(ctx context.Context, script Script, metricsPath string) (string, map[string]FlowResult, error) {
	fc := NewFlowContext()
	results := make(map[string]FlowResult, len(r.Flows))
	for _, flow := range r.Flows {
		fmt.Fprintf(os.Stderr, "  %s: %s flow...\n", script.ID, flow.Name())
		result, err := flow.Run(ctx, r.Client, script, fc)
		if err != nil {
			return flow.Name(), nil, err
		}
		results[flow.Name()] = result
		fc.Artifacts[flow.Name()] = result.Artifact
		t0 := result.Turn0
		fc.Turn0[flow.Name()] = &t0
	}

	doc, err := r.assemble(script, results)
	if err != nil {
		return "", nil, err
	}
	return "", results, WriteJSON(metricsPath, doc)
}

func writeExperimentError(script Script, flow string, err error) error {
	if script.Dir == "" {
		return nil
	}
	return WriteJSON(filepath.Join(script.Dir, ErrorFile), ExperimentError{
		ExperimentID: script.ID,
		Flow:         flow,
		Error:        err.Error(),
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
	})
}

func (r *Runner) assemble(script Script, results map[string]FlowResult) (any, error) {
	if r.Assemble != nil {
		return r.Assemble(script, results)
	}
	doc := AssembleDefault(r.Client.Model, r.Flows, script, results)
	doc.Provider = r.Client.ProviderName()
	return doc, nil
}

// DefaultMetrics is the generic per-script metrics document written when
// [Runner.Assemble] is nil. Flows is keyed by flow name; Comparison compares
// each non-first flow against the first flow in the runner's order.
type DefaultMetrics struct {
	ExperimentID string                `json:"experiment_id"`
	Model        string                `json:"model"`
	Provider     string                `json:"provider"`
	Timestamp    string                `json:"timestamp"`
	Format       string                `json:"format"`
	Flows        map[string]FlowReport `json:"flows"`
	Comparison   map[string]Comparison `json:"comparison,omitempty"`
}

// FlowReport is one flow's entry in [DefaultMetrics]: the turn-0 metrics,
// the aggregated flow metrics, the edit-turn reliability report, and any
// flow-level Extra values flattened into the same JSON object. Extra keys
// must not collide with turn0, reliability, or the FlowMetrics field names.
type FlowReport struct {
	Turn0 TurnMetrics
	FlowMetrics
	// Reliability classifies failed edit turns. It is nil when the flow had
	// no edit turns. For flows that return raw artifacts it can only count
	// request failures.
	Reliability *Reliability
	Extra       map[string]any
}

type flowReportFixed struct {
	Turn0 TurnMetrics `json:"turn0"`
	FlowMetrics
}

const reliabilityKey = "reliability"

var flowReportFixedKeys = map[string]bool{
	"turn0":                     true,
	"per_turn":                  true,
	"total_input_tokens":        true,
	"total_output_tokens":       true,
	"total_cached_input_tokens": true,
	"total_latency_ms":          true,
}

// MarshalJSON flattens Extra into the same JSON object as the fixed fields.
func (f FlowReport) MarshalJSON() ([]byte, error) {
	fixed, err := json.Marshal(flowReportFixed{Turn0: f.Turn0, FlowMetrics: f.FlowMetrics})
	if err != nil {
		return nil, err
	}
	extra := f.Extra
	if f.Reliability != nil {
		if _, clash := extra[reliabilityKey]; clash {
			return nil, fmt.Errorf("extra key %q collides with a fixed field", reliabilityKey)
		}
		extra = make(map[string]any, len(f.Extra)+1)
		for k, v := range f.Extra {
			extra[k] = v
		}
		extra[reliabilityKey] = f.Reliability
	}
	return appendExtra(fixed, extra, flowReportFixedKeys)
}

// UnmarshalJSON restores the fixed fields and collects unknown keys into
// Extra, inverting MarshalJSON. A "reliability" value that does not decode
// as a [Reliability] report stays in Extra.
func (f *FlowReport) UnmarshalJSON(data []byte) error {
	var fixed flowReportFixed
	if err := json.Unmarshal(data, &fixed); err != nil {
		return err
	}
	extra, err := splitExtra(data, flowReportFixedKeys)
	if err != nil {
		return err
	}
	var reliability *Reliability
	if raw, ok := extra[reliabilityKey]; ok {
		if r, ok := decodeReliability(raw); ok {
			reliability = r
			delete(extra, reliabilityKey)
			if len(extra) == 0 {
				extra = nil
			}
		}
	}
	*f = FlowReport{Turn0: fixed.Turn0, FlowMetrics: fixed.FlowMetrics, Reliability: reliability, Extra: extra}
	return nil
}

// decodeReliability converts a generically decoded value back into a
// Reliability report, rejecting values with unknown fields.
func decodeReliability(v any) (*Reliability, bool) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var r Reliability
	if err := dec.Decode(&r); err != nil {
		return nil, false
	}
	return &r, true
}

// AssembleDefault builds the [DefaultMetrics] document for one script.
// flows supplies the ordering: the first flow is the comparison baseline.
// Each comparison uses [CompareTurns], so edit turns that failed in either
// flow do not count toward the savings.
func AssembleDefault(model string, flows []Flow, script Script, results map[string]FlowResult) DefaultMetrics {
	doc := DefaultMetrics{
		ExperimentID: script.ID,
		Model:        model,
		Provider:     "openai-compatible",
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		Format:       script.Format,
		Flows:        make(map[string]FlowReport, len(results)),
	}
	var baseline []TurnResult
	haveBaseline := false
	for i, flow := range flows {
		result, ok := results[flow.Name()]
		if !ok {
			continue
		}
		metrics := ToFlowMetrics(result.Turns)
		report := FlowReport{
			Turn0:       result.Turn0,
			FlowMetrics: metrics,
			Extra:       result.Extra,
		}
		if _, custom := result.Extra[reliabilityKey]; len(result.Turns) > 0 && !custom {
			report.Reliability = ComputeReliability(result.Turns)
		}
		doc.Flows[flow.Name()] = report
		switch {
		case i == 0:
			baseline, haveBaseline = result.Turns, true
		case haveBaseline:
			if doc.Comparison == nil {
				doc.Comparison = map[string]Comparison{}
			}
			doc.Comparison[flow.Name()] = CompareTurns(baseline, result.Turns)
		}
	}
	return doc
}
