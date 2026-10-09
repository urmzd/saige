package eval

import "log/slog"

// Config holds options for [Run], [PopulateAll], [Compare], and
// [Experiment.Run]. Each function reads the fields that apply to it and
// ignores the rest.
type Config struct {
	// provenance, when set by WithProvenance, is compared for drift.
	provenance *[2]Provenance

	Concurrency int
	Logger      *slog.Logger
	Sampler     Sampler

	// Name names a comparison; [Run] and [Experiment.Run] take their name
	// explicitly and ignore it.
	Name string
	// OutputDir, when set, is where [Compare] writes its result with
	// [WriteComparison].
	OutputDir string
	// Resamples and Confidence configure the paired bootstrap interval of a
	// comparison and the confidence level of every interval it reports.
	// Zero values mean [DefaultResamples] and [DefaultConfidence].
	Resamples  int
	Confidence float64
	// Repeats runs the subject this many times per observation in
	// [Compare] and [Experiment.Run], numbering the copies in
	// [Observation.Sample]. Values below 2 run it once.
	Repeats int
	// Assertions gate the suite after scoring; see [SuiteResult.Gate].
	Assertions []Assertion
	// LowerIsBetter names metrics, such as latencies, where a drop is an
	// improvement when a comparison classifies per-case changes.
	LowerIsBetter map[string]bool
}

func newConfig(opts []Option) *Config {
	cfg := &Config{
		Concurrency: 1,
		Logger:      slog.Default(),
	}
	for _, o := range opts {
		if o != nil {
			o(cfg)
		}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return cfg
}

// Option configures an evaluation run or comparison.
type Option func(*Config)

// WithConcurrency sets the max parallel observation goroutines.
func WithConcurrency(n int) Option {
	return func(c *Config) {
		if n > 0 {
			c.Concurrency = n
		}
	}
}

// WithLogger sets the logger for the evaluation run.
func WithLogger(l *slog.Logger) Option {
	return func(c *Config) { c.Logger = l }
}

// WithSampler scores every observation s.N times with each scorer and reports
// the spread in [Score.Samples]. Scorers marked [Deterministic] are scored
// once. To sample only some scorers, wrap those with [Sampled] instead.
func WithSampler(s Sampler) Option {
	return func(c *Config) { c.Sampler = s }
}

// WithName names a comparison.
func WithName(name string) Option {
	return func(c *Config) { c.Name = name }
}

// WithOutputDir sets the directory for persisting comparison results.
func WithOutputDir(dir string) Option {
	return func(c *Config) { c.OutputDir = dir }
}

// WithBootstrap sets the number of resamples and the confidence level of the
// paired bootstrap interval in [Comparison.DeltaStats]. The confidence level
// also applies to the pass-rate intervals of the comparison.
func WithBootstrap(resamples int, confidence float64) Option {
	return func(c *Config) {
		c.Resamples = resamples
		c.Confidence = confidence
	}
}

// WithRepeats runs the subject n times per observation, so the variance of
// a non-deterministic subject shows up in the results. Copies keep their ID
// and are numbered 1..n in [Observation.Sample], which is what per-question
// pass rates and pass@k group by. It applies to [Compare] and
// [Experiment.Run]; [Run] scores the observations it is given.
func WithRepeats(n int) Option {
	return func(c *Config) { c.Repeats = n }
}

// WithAssertions gates the suite after scoring: [Run] calls
// [SuiteResult.Gate] with these assertions, which stamps [Score.Passed] and
// sets [SuiteResult.Outcome]. A failed gate is a result, not an error.
func WithAssertions(assertions ...Assertion) Option {
	return func(c *Config) { c.Assertions = append(c.Assertions, assertions...) }
}

// WithLowerIsBetter marks metrics where a lower value is an improvement, so
// a comparison reports a rise as a regression.
func WithLowerIsBetter(metrics ...string) Option {
	return func(c *Config) {
		if c.LowerIsBetter == nil {
			c.LowerIsBetter = make(map[string]bool, len(metrics))
		}
		for _, m := range metrics {
			c.LowerIsBetter[m] = true
		}
	}
}

// ExperimentConfig is the former name of [Config].
//
// Deprecated: Use [Config].
type ExperimentConfig = Config

// ExperimentOption is the former name of [Option].
//
// Deprecated: Use [Option].
type ExperimentOption = Option

// WithExperimentName sets the comparison name.
//
// Deprecated: Use [WithName].
func WithExperimentName(name string) Option { return WithName(name) }

// WithExperimentLogger sets the logger.
//
// Deprecated: Use [WithLogger].
func WithExperimentLogger(l *slog.Logger) Option { return WithLogger(l) }

// WithRunOptions applies opts. Comparisons now take run options such as
// [WithConcurrency] directly.
//
// Deprecated: Pass the options directly.
func WithRunOptions(opts ...Option) Option {
	return func(c *Config) {
		for _, o := range opts {
			if o != nil {
				o(c)
			}
		}
	}
}

// WithProvenance gives a comparison the provenance of its base and exp runs,
// so [Comparison.Warnings] reports catalog configuration drift (see
// [ConfigDrift]).
func WithProvenance(base, exp Provenance) Option {
	return func(c *Config) { c.provenance = &[2]Provenance{base, exp} }
}
