package eval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"runtime"
	"slices"
	"strconv"

	"github.com/urmzd/saige/agent/types"
)

// ExperimentSetup is the declarative setup captured before subjects run.
// Revision depends on inputs and settings, never measured outcomes or timings.
type ExperimentSetup struct {
	Mode               string            `json:"mode"`
	Revision           string            `json:"revision"`
	DefinitionRevision string            `json:"definition_revision,omitempty"`
	Name               string            `json:"name"`
	Claim              string            `json:"claim,omitempty"`
	Config             json.RawMessage   `json:"config,omitempty"`
	DatasetRevision    string            `json:"dataset_revision"`
	Variants           []VariantSetup    `json:"variants"`
	Scorers            []ScorerSetup     `json:"scorers"`
	Assertions         []Assertion       `json:"assertions,omitempty"`
	Sampling           SamplingSetup     `json:"sampling"`
	SubjectRepeats     int               `json:"subject_repeats"`
	EvaluationSamples  int               `json:"evaluation_samples"`
	Concurrency        int               `json:"concurrency"`
	Measurements       bool              `json:"measurements"`
	GatePolicy         GatePolicy        `json:"gate_policy"`
	DialPolicy         *types.DialPolicy `json:"dial_policy,omitempty"`
}

// VariantSetup records the subject revision and the knobs changed by an arm.
type VariantSetup struct {
	Name               string          `json:"name"`
	Revision           string          `json:"revision"`
	DefinitionRevision string          `json:"definition_revision,omitempty"`
	Labels             Labels          `json:"labels,omitempty"`
	Config             json.RawMessage `json:"config,omitempty"`
}

// ScorerSetup pins one metric's implementation and configuration.
type ScorerSetup struct {
	Config   json.RawMessage `json:"config,omitempty"`
	Name     string          `json:"name"`
	Revision string          `json:"revision"`
}

// SamplingSetup records how every scoring call is repeated and reduced.
type SamplingSetup struct {
	N           int     `json:"n"`
	Concurrency int     `json:"concurrency"`
	Tolerance   float64 `json:"tolerance"`
	Confidence  float64 `json:"confidence"`
	Reducer     string  `json:"reducer"`
}

// Snapshot captures effective options without running subjects or evaluators.
// Config must describe captured settings that function identity cannot reveal.
func (e Experiment) Snapshot(dataset []Observation, evaluations int, opts ...Option) (*ExperimentSetup, error) {
	return e.snapshot(dataset, evaluations, true, opts...)
}

func (e Experiment) snapshot(dataset []Observation, evaluations int, subjects bool, opts ...Option) (*ExperimentSetup, error) {
	if err := e.validateSetup(subjects); err != nil {
		return nil, err
	}
	if evaluations < 1 {
		return nil, fmt.Errorf("experiment: evaluations must be positive")
	}
	cfg := newConfig(e.runOptions(opts))
	setup := &ExperimentSetup{
		Mode:               "subjects",
		DefinitionRevision: e.Revision, Name: e.Name, Claim: e.Claim, Config: canonicalConfig(e.Config),
		Assertions: cfg.Assertions, SubjectRepeats: max(1, cfg.Repeats), EvaluationSamples: evaluations,
		Concurrency: cfg.Concurrency, Measurements: cfg.Measurements, GatePolicy: cfg.GatePolicy, DialPolicy: cfg.dialPolicy,
		Sampling: SamplingSetup{N: max(1, cfg.Sampler.N), Concurrency: max(1, cfg.Sampler.Concurrency),
			Tolerance: cfg.Sampler.Tolerance, Confidence: cfg.Sampler.Confidence, Reducer: "mean"},
	}
	if setup.Sampling.Confidence <= 0 || setup.Sampling.Confidence >= 1 {
		setup.Sampling.Confidence = DefaultConfidence
	}
	if cfg.Sampler.Reduce != nil {
		setup.Sampling.Reducer = runtime.FuncForPC(reflect.ValueOf(cfg.Sampler.Reduce).Pointer()).Name()
	}
	inputs := make([]string, len(dataset))
	for i, obs := range dataset {
		inputs[i] = RevisionTag(obs.Revision, observationRevision(obs))
	}
	setup.DatasetRevision = RevisionTag("dataset/v1", inputs)
	for _, v := range e.Variants {
		implementation := "fixed_trace"
		if v.Subject != nil {
			implementation = runtime.FuncForPC(reflect.ValueOf(v.Subject).Pointer()).Name()
		}
		revision := RevisionTag("variant/v1", v.Name, v.Revision, v.Labels, canonicalConfig(v.Config), implementation)
		setup.Variants = append(setup.Variants, VariantSetup{Name: v.Name, Revision: revision, DefinitionRevision: v.Revision, Labels: v.Labels, Config: canonicalConfig(v.Config)})
	}
	for _, sc := range e.Scorers {
		sc = Sampled(sc, cfg.Sampler)
		setup.Scorers = append(setup.Scorers, ScorerSetup{Name: sc.Name(), Revision: ScorerRevision(sc), Config: ScorerConfiguration(sc)})
	}
	if !subjects {
		setup.Mode = "fixed_traces"
		setup.SubjectRepeats = 0
	}
	raw, err := json.Marshal(setup)
	if err != nil {
		return nil, fmt.Errorf("experiment setup: %w", err)
	}
	// Round-trip before hashing to detach caller-owned maps and normalize JSON.
	if err := json.Unmarshal(raw, &setup); err != nil {
		return nil, err
	}
	setup.Revision = RevisionTag("experiment/v1", setup)
	return setup, nil
}

func (e Experiment) runOptions(opts []Option) []Option {
	out := []Option{WithMeasurements()}
	if e.Sampler.N > 1 {
		out = append(out, WithSampler(e.Sampler))
	}
	out = append(out, opts...)
	return append(out, WithAssertions(e.Assertions...))
}

func cloneSetup(setup *ExperimentSetup) *ExperimentSetup {
	if setup == nil {
		return nil
	}
	out := *setup
	out.Config = slices.Clone(setup.Config)
	out.Variants = slices.Clone(setup.Variants)
	for i := range out.Variants {
		out.Variants[i].Labels = out.Variants[i].Labels.Clone()
		out.Variants[i].Config = slices.Clone(out.Variants[i].Config)
	}
	out.Scorers = slices.Clone(setup.Scorers)
	for i := range out.Scorers {
		out.Scorers[i].Config = slices.Clone(out.Scorers[i].Config)
	}
	out.Assertions = slices.Clone(setup.Assertions)
	for i := range out.Assertions {
		out.Assertions[i].Where = out.Assertions[i].Where.clone()
	}
	if setup.DialPolicy != nil {
		p := *setup.DialPolicy
		p.Per = maps.Clone(setup.DialPolicy.Per)
		out.DialPolicy = &p
	}
	return &out
}

// SetupChange explains a changed experimental setting alongside score deltas.
type SetupChange struct {
	Field  string          `json:"field"`
	Before json.RawMessage `json:"before,omitempty"`
	After  json.RawMessage `json:"after,omitempty"`
}

// DiffSetups reports each changed setting alongside its old and new values.
// Revision fields are retained except the derived top-level setup revision.
func DiffSetups(base, candidate *ExperimentSetup) []SetupChange {
	if base == nil || candidate == nil {
		return nil
	}
	decode := func(s *ExperimentSetup) map[string]any {
		raw, _ := json.Marshal(s)
		var m map[string]any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		_ = decoder.Decode(&m)
		delete(m, "revision")
		return m
	}
	var changes []SetupChange
	var visit func(string, any, any)
	visit = func(path string, b, c any) {
		bm, bok := b.(map[string]any)
		cm, cok := c.(map[string]any)
		if bok && cok {
			keys := make([]string, 0, len(bm)+len(cm))
			for k := range bm {
				keys = append(keys, k)
			}
			for k := range cm {
				if _, ok := bm[k]; !ok {
					keys = append(keys, k)
				}
			}
			slices.Sort(keys)
			for _, k := range keys {
				next := k
				if path != "" {
					next = path + "." + k
				}
				visit(next, bm[k], cm[k])
			}
			return
		}
		ba, bok := b.([]any)
		ca, cok := c.([]any)
		if bok && cok && len(ba) == len(ca) {
			for i := range ba {
				visit(path+"["+strconv.Itoa(i)+"]", ba[i], ca[i])
			}
			return
		}
		before, _ := json.Marshal(b)
		after, _ := json.Marshal(c)
		if !bytes.Equal(before, after) {
			changes = append(changes, SetupChange{Field: path, Before: before, After: after})
		}
	}
	visit("", decode(base), decode(candidate))
	return changes
}

func canonicalConfig(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return raw
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return raw
	}
	return canonical
}
