package eval

import (
	"context"
	"log/slog"
	"time"
)

// ExperimentConfig configures an A/B experiment.
type ExperimentConfig struct {
	Name      string
	OutputDir string
	Logger    *slog.Logger
}

// ExperimentOption configures an experiment.
type ExperimentOption func(*ExperimentConfig)

// WithExperimentName sets the experiment name.
func WithExperimentName(name string) ExperimentOption {
	return func(c *ExperimentConfig) { c.Name = name }
}

// WithOutputDir sets the directory for persisting experiment results.
func WithOutputDir(dir string) ExperimentOption {
	return func(c *ExperimentConfig) { c.OutputDir = dir }
}

// WithExperimentLogger sets the logger.
func WithExperimentLogger(l *slog.Logger) ExperimentOption {
	return func(c *ExperimentConfig) { c.Logger = l }
}

// ExperimentResult holds the complete A/B comparison.
type ExperimentResult struct {
	Name          string              `json:"name"`
	CreatedAt     time.Time           `json:"created_at"`
	BaseResults   []ObservationResult `json:"base_results"`
	ExpResults    []ObservationResult `json:"exp_results"`
	BaseAggregate map[string]float64  `json:"base_aggregate"`
	ExpAggregate  map[string]float64  `json:"exp_aggregate"`
	Deltas        map[string]float64  `json:"deltas"`
	// BaseErroredCases and ExpErroredCases count observations with at least
	// one errored score on each side; errored scores are excluded from the
	// aggregates and deltas.
	BaseErroredCases int `json:"base_errored_cases,omitempty"`
	ExpErroredCases  int `json:"exp_errored_cases,omitempty"`
}

// RunExperiment runs both subjects on the same inputs, scores them, and
// computes the delta between experimental and base aggregate metrics.
func RunExperiment(ctx context.Context, inputs []Observation, base, exp Subject, scorers []Scorer, opts ...ExperimentOption) (*ExperimentResult, error) {
	cfg := &ExperimentConfig{
		Name:   "experiment",
		Logger: slog.Default(),
	}
	for _, o := range opts {
		o(cfg)
	}

	// Deep-copy inputs for each subject so they don't interfere.
	baseObs := copyObservations(inputs)
	expObs := copyObservations(inputs)

	// Run base subject.
	if err := Populate(ctx, baseObs, base); err != nil {
		return nil, err
	}
	// Run experimental subject.
	if err := Populate(ctx, expObs, exp); err != nil {
		return nil, err
	}

	// Score both.
	baseSuite, err := Run(ctx, cfg.Name+"/base", baseObs, scorers)
	if err != nil {
		return nil, err
	}
	expSuite, err := Run(ctx, cfg.Name+"/exp", expObs, scorers)
	if err != nil {
		return nil, err
	}

	// Compute deltas (exp - base).
	deltas := make(map[string]float64)
	for name, expVal := range expSuite.Aggregate {
		if baseVal, ok := baseSuite.Aggregate[name]; ok {
			deltas[name] = expVal - baseVal
		} else {
			deltas[name] = expVal
		}
	}

	result := &ExperimentResult{
		Name:             cfg.Name,
		CreatedAt:        time.Now(),
		BaseResults:      baseSuite.Results,
		ExpResults:       expSuite.Results,
		BaseAggregate:    baseSuite.Aggregate,
		ExpAggregate:     expSuite.Aggregate,
		Deltas:           deltas,
		BaseErroredCases: baseSuite.ErroredCases,
		ExpErroredCases:  expSuite.ErroredCases,
	}

	// Persist if output dir is set.
	if cfg.OutputDir != "" {
		if err := WriteExperiment(cfg.OutputDir, result); err != nil {
			cfg.Logger.Error("failed to write experiment", "dir", cfg.OutputDir, "error", err)
		}
	}

	return result, nil
}

func copyObservations(obs []Observation) []Observation {
	out := make([]Observation, len(obs))
	for i, o := range obs {
		out[i] = Observation{
			ID:          o.ID,
			Turn:        o.Turn,
			Input:       append([]byte(nil), o.Input...),
			GroundTruth: append([]byte(nil), o.GroundTruth...),
		}
	}
	return out
}
