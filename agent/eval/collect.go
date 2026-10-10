package eval

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/urmzd/saige/agent/types"
	topeval "github.com/urmzd/saige/eval"
)

// AgentRun is everything an agent eval needs from one drained delta stream.
type AgentRun struct {
	// Text is the concatenated assistant text.
	Text string
	// Timing holds latency, token usage, and any stream errors.
	Timing StreamTiming
	// ToolCalls lists the top-level tool calls in the order the model
	// started them, with arguments, results, errors, and execution time.
	ToolCalls []ToolCallRecord
	// TurnCount is the number of provider calls, counted from the
	// UsageDelta the agent loop emits once per loop iteration.
	TurnCount int
	// TotalMs is the time from start until the stream closed.
	TotalMs int64
	// Usage is the billable token usage of the run's provider calls, one
	// call per top-level UsageDelta. A response-cache replay adds nothing.
	Usage types.TokenUsage
	// Models lists the response models the provider reported, in first-seen
	// order, for [topeval.Provenance.AddModels].
	Models []string
	// CostUSD is the run's cost, set by [AgentRun.Priced]. Nil means no
	// price was applied or the run could not be priced.
	CostUSD *float64
	// Routes lists the configuration that served each provider call, in
	// order: the last top-level route reported before the call's usage.
	// Empty when the provider reports no routes.
	Routes []RouteRecord
	// Parts are the assistant parts of the final provider call, in order:
	// its text, thinking, citations, refusal and media output. Text is
	// their text projection over the whole run.
	Parts []types.AssistantPart
	// Media identifies the media parts of the final call, without bytes.
	Media []MediaRecord
	// Conversions lists every executed conversion decision of the run's
	// provider calls, in order: how each media part reached the model.
	Conversions []types.ConversionDecision
	// Citations lists the sources the run's tools cited, numbered by the
	// agent's citation registry. Citations the model made are
	// CitationParts in Parts.
	Citations []types.Citation
	// Deltas holds every delta received, for further inspection.
	Deltas []types.Delta
}

// RouteRecord names the configuration that served one provider call.
type RouteRecord struct {
	Profile         string `json:"profile,omitempty"`
	Model           string `json:"model,omitempty"`
	Preset          string `json:"preset,omitempty"`
	ConfigHash      string `json:"config_hash,omitempty"`
	CatalogRevision string `json:"catalog_revision,omitempty"`
	// Dials records how the call's dials compiled: the effective raw
	// options and each decision. Nil when the call carried no dials.
	Dials *types.DialReport `json:"dials,omitempty"`
	// Conversions is the call's planned media conversions, as the route
	// reported them. Nil when the call converted nothing.
	Conversions *types.ConversionReport `json:"conversions,omitempty"`
}

// AddProvenance records the run's response models, serving catalog
// configurations, what each dial was sent as, the versions of the tools it
// ran, and how its media reached the models on p.
func (r AgentRun) AddProvenance(p *topeval.Provenance) {
	p.AddModels(r.Models...)
	for _, d := range r.Conversions {
		p.AddConversion(string(d.Kind), d.Action, d.Via)
	}
	for _, c := range r.ToolCalls {
		p.AddTool(c.Name, c.Version)
	}
	for _, rt := range r.Routes {
		p.AddRoute(rt.Profile, rt.Preset, rt.ConfigHash, rt.CatalogRevision)
		if rt.Dials == nil {
			continue
		}
		for _, d := range rt.Dials.Decisions {
			p.AddDial(string(d.Dial), d.Requested, dialSent(d))
		}
	}
}

// dialSent is what a decision sent, for provenance: the raw parameters, or
// the action when nothing was sent.
func dialSent(d types.DialDecision) string {
	switch {
	case d.Action == types.DialDropped || d.Action == types.DialRejected:
		return string(d.Action)
	case d.Sent == "":
		return "nothing"
	}
	return d.Sent
}

// CollectAgentRun drains ch into an [AgentRun], starting the clock now.
// Prefer [CollectAgentRunFrom] when the stream was started earlier.
func CollectAgentRun(ch <-chan types.Delta) AgentRun {
	return CollectAgentRunFrom(time.Now(), ch)
}

// CollectAgentRunFrom drains ch into an [AgentRun] with latency measured from
// start.
//
// Tool calls are built in one pass. Each tool call part's end supplies its
// arguments or the error from parsing them. ToolExecStartDelta and ToolExecEndDelta are joined
// by tool call ID to fill the result, the error, and the wall-clock execution
// time. Deltas nested in ToolExecDelta belong to sub-agents and streaming
// tools, so they are kept in Deltas but do not enter the top-level
// trajectory, text, or timing.
func CollectAgentRunFrom(start time.Time, ch <-chan types.Delta) AgentRun {
	var (
		run       AgentRun
		sc        streamCollector
		tc        toolCollector
		pc        partCollector
		lastDelta = start
		route     *types.RouteDelta
	)
	for delta := range ch {
		now := time.Now()
		lastDelta = now
		run.Deltas = append(run.Deltas, delta)
		sc.observe(now, delta)
		tc.observe(now, delta)
		pc.observe(delta)
		switch v := delta.(type) {
		case types.RouteDelta:
			route = &v
		case types.ConversionDelta:
			run.Conversions = append(run.Conversions, v.Report.Decisions...)
		case types.CitationDelta:
			run.Citations = append(run.Citations, v.Citation)
		}
		if u, ok := delta.(types.UsageDelta); ok {
			if route != nil {
				rec := RouteRecord{Profile: route.Profile, Model: route.Model, Preset: route.Preset,
					ConfigHash: route.ConfigHash, CatalogRevision: route.CatalogRevision}
				if route.Dials != nil {
					d := route.Dials.Clone()
					rec.Dials = &d
				}
				if route.Conversions != nil {
					c := route.Conversions.Clone()
					rec.Conversions = &c
				}
				run.Routes = append(run.Routes, rec)
				route = nil
			}
			run.TurnCount++
			run.Usage.Add(types.UsageFromDelta(u))
			if u.ResponseModel != "" && !slices.Contains(run.Models, u.ResponseModel) {
				run.Models = append(run.Models, u.ResponseModel)
			}
		}
	}
	run.Text = sc.text.String()
	run.Timing = sc.timing(start)
	run.ToolCalls = tc.records()
	run.Parts = pc.parts()
	run.Media = MediaRecords(run.Parts)
	run.TotalMs = lastDelta.Sub(start).Milliseconds()
	return run
}

// Priced returns a copy of the run with CostUSD computed from its usage at
// the given rates. Rates in a currency other than USD, and an unpriced rate
// card (all zero and not marked free), leave CostUSD nil, so an unknown cost
// is never reported as zero. A run that reported more than one response
// model (a fallback or routed run) also gets a nil CostUSD: Usage is summed
// across models, so one rate card cannot price it.
func (r AgentRun) Priced(p types.Pricing) AgentRun {
	if len(r.Models) > 1 || (p.IsZero() && !p.Free) || (p.Currency != "" && p.Currency != types.DefaultCurrency) {
		r.CostUSD = nil
		return r
	}
	usd := p.Cost(r.Usage).Float()
	r.CostUSD = &usd
	return r
}

// toolCollector pairs tool call and tool execution deltas into records.
type toolCollector struct {
	calls     []ToolCallRecord
	execStart []time.Time
	executed  []bool // an execution delta has claimed the record
	byID      map[string]int
	open      map[int]int // part index -> record of a call still streaming
	args      map[int]string
}

func (tc *toolCollector) add(id, name string) int {
	tc.calls = append(tc.calls, ToolCallRecord{ID: id, Name: name, Exec: ExecNotRun})
	tc.execStart = append(tc.execStart, time.Time{})
	tc.executed = append(tc.executed, false)
	idx := len(tc.calls) - 1
	if id != "" {
		if tc.byID == nil {
			tc.byID = map[string]int{}
		}
		tc.byID[id] = idx
	}
	return idx
}

// lookup returns the record for an executed call. A call announced without
// an ID is adopted by the first execution of the same tool name; a call the
// stream never announced (for example a replayed execution) gets a new
// record.
func (tc *toolCollector) lookup(id, name string) int {
	if idx, ok := tc.byID[id]; ok && id != "" {
		return idx
	}
	for idx := range tc.calls {
		rec := &tc.calls[idx]
		if rec.ID == "" && rec.Name == name && !tc.executed[idx] {
			rec.ID = id
			if id != "" {
				if tc.byID == nil {
					tc.byID = map[string]int{}
				}
				tc.byID[id] = idx
			}
			return idx
		}
	}
	return tc.add(id, name)
}

func (tc *toolCollector) observe(now time.Time, delta types.Delta) {
	switch v := delta.(type) {
	case types.PartStart:
		if v.Kind == types.KindToolCall {
			if tc.open == nil {
				tc.open, tc.args = map[int]int{}, map[int]string{}
			}
			tc.open[v.Index] = tc.add(v.ID, v.Name)
			delete(tc.args, v.Index)
		}

	case types.PartDelta:
		if _, ok := tc.open[v.Index]; ok && v.Args != "" {
			tc.args[v.Index] += v.Args
		}

	case types.PartEnd:
		idx, ok := tc.open[v.Index]
		if !ok {
			break
		}
		delete(tc.open, v.Index)
		if call, ok := v.Part.(types.ToolCallPart); ok {
			tc.calls[idx].Arguments, tc.calls[idx].ArgumentsError = call.Arguments, call.ArgumentsError
		} else {
			tc.calls[idx].Arguments, tc.calls[idx].ArgumentsError = types.DecodeToolArguments(tc.args[v.Index])
		}
		delete(tc.args, v.Index)

	case types.ToolExecStartDelta:
		idx := tc.lookup(v.ToolCallID, v.Name)
		tc.executed[idx] = true
		tc.execStart[idx] = now
		if tc.calls[idx].Exec != ExecFinished {
			tc.calls[idx].Exec = ExecUnfinished
		}

	case types.ToolExecEndDelta:
		idx := tc.lookup(v.ToolCallID, v.Name)
		tc.executed[idx] = true
		rec := &tc.calls[idx]
		if rec.Name == "" {
			rec.Name = v.Name
		}
		rec.Result = v.Result
		rec.Error = v.Error
		rec.Version = v.Version
		rec.Exec = ExecFinished
		if !tc.execStart[idx].IsZero() {
			rec.DurationMs = now.Sub(tc.execStart[idx]).Milliseconds()
		}
	}
}

func (tc *toolCollector) records() []ToolCallRecord {
	if tc.calls == nil {
		return []ToolCallRecord{}
	}
	return tc.calls
}

// AnnotateObservation records an [AgentRun] on obs under the agent
// annotation keys ([AnnotationToolCalls], [AnnotationTurnCount],
// [AnnotationStreamTiming], [AnnotationParts], and, when the run has them,
// [AnnotationMedia], [AnnotationConversions] and [AnnotationCitations]) so
// the scorers in this package can read it. Parts are recorded without media
// bytes. It
// also fills obs.Timing, including the cost when the run was [AgentRun.Priced],
// and, when obs.Output is empty, sets Output to the run's text as a JSON
// string.
func AnnotateObservation(obs *topeval.Observation, run AgentRun) error {
	calls := run.ToolCalls
	if calls == nil {
		calls = []ToolCallRecord{}
	}
	values := map[string]any{
		AnnotationToolCalls:    calls,
		AnnotationTurnCount:    run.TurnCount,
		AnnotationStreamTiming: run.Timing,
	}
	if obs.Annotations == nil {
		obs.Annotations = make(map[string]json.RawMessage, len(values))
	}
	for key, value := range values {
		raw, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("annotate %s: %w", key, err)
		}
		obs.Annotations[key] = raw
	}
	if err := AnnotateParts(obs, run.Parts); err != nil {
		return err
	}
	optional := map[string]any{}
	if len(run.Conversions) > 0 {
		optional[AnnotationConversions] = run.Conversions
	}
	if len(run.Citations) > 0 {
		optional[AnnotationCitations] = run.Citations
	}
	for key, value := range optional {
		raw, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("annotate %s: %w", key, err)
		}
		obs.Annotations[key] = raw
	}

	if len(obs.Output) == 0 {
		out, err := json.Marshal(run.Text)
		if err != nil {
			return fmt.Errorf("annotate output: %w", err)
		}
		obs.Output = out
	}
	obs.Timing = topeval.ObservationTiming{
		TotalMs:      run.TotalMs,
		TTFTMs:       run.Timing.TTFTMs,
		TTLTMs:       run.Timing.TTLTMs,
		MedianITL:    run.Timing.MedianITL,
		InputTokens:  run.Timing.InputTokens,
		OutputTokens: run.Timing.OutputTokens,
	}
	if run.CostUSD != nil {
		obs.Timing.SetCostUSD(*run.CostUSD)
	}
	return nil
}
