package eval

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/urmzd/saige/eval/analysis"
)

// ExperimentSamples retains independently executed evaluations of fixed outputs.
// SubjectCostUSD counts producing those outputs once, not once per evaluation.
type ExperimentSamples struct {
	ID                string                     `json:"id"`
	Setup             *ExperimentSetup           `json:"setup"`
	Evaluations       []*SuiteResult             `json:"evaluations"`
	Metrics           map[string]Stat            `json:"metrics"`
	Variants          map[string]map[string]Stat `json:"variants"`
	SubjectCostUSD    *float64                   `json:"subject_cost_usd,omitempty"`
	EvaluationCost    Stat                       `json:"evaluation_cost"`
	EvaluationLatency Stat                       `json:"evaluation_latency_ms"`
}

type evaluationTrialKey struct{}
type evaluationTrial struct {
	run    string
	number int
}

// Sample populates each subject once (or WithRepeats times), then executes
// the entire evaluation n times on those same outputs: scorers, metadata and
// assertions run afresh every trial. It never resamples stored score values.
// WithSampler independently controls repeated calls within each scorer.
// Disable response caches to collect fresh model draws.
func (e Experiment) Sample(ctx context.Context, dataset []Observation, n int, opts ...Option) (*ExperimentSamples, error) {
	setup, err := e.Snapshot(dataset, n, opts...)
	if err != nil {
		return nil, err
	}
	runOpts := e.runOptions(opts)
	cfg := newConfig(runOpts)
	dataset = Sampler{N: cfg.Repeats}.Replicate(dataset)
	var all []Observation
	for _, v := range e.Variants {
		obs := copyObservations(dataset)
		labels := v.Labels.merge(Labels{LabelVariant: v.Name})
		for i := range obs {
			obs[i].Labels = obs[i].Labels.merge(labels)
		}
		_ = PopulateAll(ctx, obs, v.Subject, runOpts...)
		all = append(all, obs...)
	}
	return e.evaluatePrepared(ctx, all, n, setup, runOpts)
}

// Evaluate executes complete evaluation trials over existing traces, without
// invoking a subject. Traces must identify a declared variant; for a single
// variant its label is filled automatically. Subject repeats do not apply.
func (e Experiment) Evaluate(ctx context.Context, traces []Observation, n int, opts ...Option) (*ExperimentSamples, error) {
	setup, err := e.snapshot(traces, n, false, opts...)
	if err != nil {
		return nil, err
	}
	runOpts := e.runOptions(opts)
	if newConfig(runOpts).Repeats > 1 {
		return nil, fmt.Errorf("experiment Evaluate: WithRepeats regenerates subjects; supply fixed traces instead")
	}
	declared := map[string]Variant{}
	for _, v := range e.Variants {
		declared[v.Name] = v
	}
	observations := copyObservations(traces)
	for i, trace := range traces {
		name := trace.Labels[LabelVariant]
		if name == "" && len(e.Variants) == 1 {
			name = e.Variants[0].Name
		}
		variant, ok := declared[name]
		if !ok {
			return nil, fmt.Errorf("experiment Evaluate: trace %q has undeclared variant %q", trace.ID, name)
		}
		observations[i].Labels = observations[i].Labels.merge(variant.Labels).merge(Labels{LabelVariant: name})
		observations[i].Output = append([]byte(nil), trace.Output...)
		observations[i].Timing = cloneTiming(trace.Timing)
	}
	return e.evaluatePrepared(ctx, observations, n, setup, runOpts)
}

func (e Experiment) evaluatePrepared(ctx context.Context, all []Observation, n int, setup *ExperimentSetup, runOpts []Option) (*ExperimentSamples, error) {
	cfg := newConfig(runOpts)
	result := &ExperimentSamples{ID: NewRunID(), Setup: setup}
	var errs []error
	for i := range n {
		trialCtx := context.WithValue(ctx, evaluationTrialKey{}, evaluationTrial{run: result.ID, number: i + 1})
		// A fresh copy prevents a custom scorer from modifying the next trial.
		observations := copyObservations(all)
		for j := range observations {
			observations[j].Output = append([]byte(nil), all[j].Output...)
			observations[j].Timing = cloneTiming(all[j].Timing)
		}
		suite, runErr := Run(trialCtx, e.Name, observations, e.Scorers, runOpts...)
		suite.Claim = e.Claim
		suite.Setup = cloneSetup(setup)
		suite.Revision = setup.Revision
		suite.Evaluation = i + 1
		if len(cfg.Assertions) == 0 {
			suite.Gate()
		}
		result.Evaluations = append(result.Evaluations, suite)
		if runErr != nil {
			errs = append(errs, fmt.Errorf("evaluation %d: %w", i+1, runErr))
		}
		if ctx.Err() != nil {
			break
		}
	}
	subjectResults := make([]ObservationResult, len(all))
	for i, obs := range all {
		subjectResults[i].Observation = obs
	}
	result.SubjectCostUSD = TotalCostUSD(subjectResults)
	result.summarize(n, setup.Sampling.Confidence)
	return result, errors.Join(errs...)
}

func (s *ExperimentSamples) summarize(expected int, confidence float64) {
	metrics := map[string][]float64{}
	variants := map[string]map[string][]float64{}
	var latency, cost []float64
	for _, trial := range s.Evaluations {
		if trial.Incomplete > 0 {
			continue
		}
		for name, v := range trial.Aggregate {
			metrics[name] = append(metrics[name], v)
		}
		groups := map[string][]ObservationResult{}
		for _, r := range trial.Results {
			v := r.Observation.Labels[LabelVariant]
			groups[v] = append(groups[v], r)
		}
		for variant, rs := range groups {
			if variants[variant] == nil {
				variants[variant] = map[string][]float64{}
			}
			for metric, v := range Aggregate(rs) {
				variants[variant][metric] = append(variants[variant][metric], v)
			}
		}
		latency = append(latency, float64(trial.EvaluationTiming.TotalMs))
		if trial.EvaluationTiming.CostUSD != nil {
			cost = append(cost, *trial.EvaluationTiming.CostUSD)
		}
	}
	s.Metrics = map[string]Stat{}
	for metric, vs := range metrics {
		s.Metrics[metric] = analysis.MeanStat(vs, expected, confidence)
	}
	s.Variants = map[string]map[string]Stat{}
	for variant, ms := range variants {
		s.Variants[variant] = map[string]Stat{}
		for metric, vs := range ms {
			s.Variants[variant][metric] = analysis.MeanStat(vs, expected, confidence)
		}
	}
	s.EvaluationLatency = analysis.MeanStat(latency, expected, confidence)
	s.EvaluationCost = analysis.MeanStat(cost, expected, confidence)
}

func trialBatchKey(ctx context.Context, key string) string {
	if trial, ok := ctx.Value(evaluationTrialKey{}).(evaluationTrial); ok {
		return trial.run + "/evaluation/" + strconv.Itoa(trial.number) + "/" + key
	}
	return key
}
