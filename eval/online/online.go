// Package online scores production agent runs after they finish.
//
// A [Source] yields finished runs as [Record] values: from conversation trees
// in memory ([TreeSource]), from conversations stored by agent/pgstore
// ([PGSource]), or from route and trace records collected with agent/eval
// ([RecordSource] over [FromAgentRun]). A [Sampler] keeps the records that
// pass its [Filter], samples a deterministic fraction of them, scores those
// with ordinary eval scorers (deterministic checks, plus LLM judges charged
// to a budget), and writes one [eval.RunRecord] with one [eval.Unit] per
// scored record to an eval/store.Store, labeled [SourceOnline].
//
// Every unit links back to what produced it: the observation ID is the node
// that ended the run, and the labels [LabelConversation] and [LabelNode]
// name the conversation and node. A record with a trace ID also sets
// [eval.Unit.Trace].
//
// [Sampler.Sweep] scores one time window once. [Sampler.Watch] runs until
// its context ends, scoring each run a producer announces with [Announce]
// on a types.Notifier, such as postgres.Notifier across processes.
// [Promote] turns failing or flagged units into dataset cases with personal
// data redacted.
package online

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"

	agenteval "github.com/urmzd/saige/agent/eval"
	"github.com/urmzd/saige/eval"
)

// Labels every online unit carries.
const (
	// LabelSource is set to [SourceOnline] on online runs and units.
	LabelSource = "source"
	// SourceOnline marks results scored from production runs.
	SourceOnline = "online"
	// LabelConversation is the conversation the run belongs to.
	LabelConversation = "conversation"
	// LabelNode is the node that ended the run.
	LabelNode = "node"
	// LabelModel and LabelPreset name the configuration that served the
	// run's last provider call, when it was recorded.
	LabelModel  = "model"
	LabelPreset = "preset"
	// LabelErrored is "true" when the run recorded an error.
	LabelErrored = "errored"
)

// AnnotationRef is the observation annotation holding the record's [Ref].
const AnnotationRef = "online.ref"

// Ref identifies one finished run: the conversation and the node that ended
// it. It is also the payload [Announce] publishes.
type Ref struct {
	Conversation string `json:"conversation"`
	Node         string `json:"node"`
	// TraceID and SpanID link the run to a trace, when it has one.
	TraceID string `json:"trace_id,omitempty"`
	SpanID  string `json:"span_id,omitempty"`
}

// Record is one finished production run, ready to score.
type Record struct {
	Ref        Ref       `json:"ref"`
	FinishedAt time.Time `json:"finished_at"`
	// Input is the user message that started the run; Output is the
	// assistant text that ended it.
	Input  string `json:"input"`
	Output string `json:"output"`
	// Model and Preset name the configuration of the run's last provider
	// call, empty when no route was recorded.
	Model  string `json:"model,omitempty"`
	Preset string `json:"preset,omitempty"`
	// ToolCalls lists the run's tool calls in order.
	ToolCalls []agenteval.ToolCallRecord `json:"tool_calls,omitempty"`
	// Turns counts the run's assistant turns (provider calls).
	Turns int `json:"turns"`
	// Error describes the first failure in the run, such as a tool error or
	// a truncated turn; empty for a clean run.
	Error string `json:"error,omitempty"`
	// Labels are extra coordinates, such as a tenant or product surface.
	Labels eval.Labels            `json:"labels,omitempty"`
	Timing eval.ObservationTiming `json:"timing"`
}

// Tools returns the distinct tool names the run called, in first-call order.
func (r Record) Tools() []string {
	var names []string
	for _, c := range r.ToolCalls {
		if !slices.Contains(names, c.Name) {
			names = append(names, c.Name)
		}
	}
	return names
}

// Observation converts the record to an [eval.Observation] that the
// agent/eval scorers can read: the tool calls and turn count are set under
// their annotation keys, Input and Output are JSON strings, and the labels
// carry the source, conversation, node, model, preset, and error flag.
func (r Record) Observation() (eval.Observation, error) {
	labels := eval.Labels{}
	maps.Copy(labels, r.Labels)
	labels[LabelSource] = SourceOnline
	labels[LabelConversation] = r.Ref.Conversation
	labels[LabelNode] = r.Ref.Node
	labels[LabelErrored] = strconv.FormatBool(r.Error != "")
	if r.Model != "" {
		labels[LabelModel] = r.Model
	}
	if r.Preset != "" {
		labels[LabelPreset] = r.Preset
	}
	obs := eval.Observation{ID: r.Ref.Node, Labels: labels, Timing: r.Timing}
	var err error
	if obs.Input, err = json.Marshal(r.Input); err != nil {
		return obs, err
	}
	if obs.Output, err = json.Marshal(r.Output); err != nil {
		return obs, err
	}
	calls := r.ToolCalls
	if calls == nil {
		calls = []agenteval.ToolCallRecord{}
	}
	obs.Annotations = map[string]json.RawMessage{}
	for key, v := range map[string]any{
		agenteval.AnnotationToolCalls: calls,
		agenteval.AnnotationTurnCount: r.Turns,
		AnnotationRef:                 r.Ref,
	} {
		raw, err := json.Marshal(v)
		if err != nil {
			return obs, fmt.Errorf("annotate %s: %w", key, err)
		}
		obs.Annotations[key] = raw
	}
	return obs, nil
}

// Window is a half-open time range [From, To) of run finish times. A zero
// To means now.
type Window struct {
	From time.Time
	To   time.Time
}

// Contains reports whether t falls in the window.
func (w Window) Contains(t time.Time) bool {
	if t.Before(w.From) {
		return false
	}
	return w.To.IsZero() || t.Before(w.To)
}
