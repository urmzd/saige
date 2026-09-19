package eval

import "log/slog"

// Config holds options for [Run].
type Config struct {
	Concurrency int
	Logger      *slog.Logger
	Sampler     Sampler
}

// Option configures an evaluation run.
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
