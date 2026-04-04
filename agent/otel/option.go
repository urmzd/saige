package otel

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/urmzd/saige/agent"
)

// Config configures OpenTelemetry tracing and metrics for SAIGE agents.
type Config struct {
	// TracerProvider to use. If nil, the global provider is used.
	TracerProvider trace.TracerProvider
	// MeterProvider to use for metrics. If nil, the global provider is used.
	MeterProvider metric.MeterProvider
	// ServiceName used for the tracer and meter. Defaults to "saige".
	ServiceName string
}

func (c Config) tracer() trace.Tracer {
	tp := c.TracerProvider
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	name := c.ServiceName
	if name == "" {
		name = "saige"
	}
	return tp.Tracer(name)
}

func (c Config) meter() metric.Meter {
	mp := c.MeterProvider
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	name := c.ServiceName
	if name == "" {
		name = "saige"
	}
	return mp.Meter(name)
}

// WithTracing returns an AgentOption that wraps the provider and tools
// with OpenTelemetry tracing, and bridges the Metrics interface to OTel.
func WithTracing(cfg Config) agent.AgentOption {
	return func(c *agent.AgentConfig) {
		tracer := cfg.tracer()

		// Wrap provider.
		if c.Provider != nil {
			c.Provider = NewTracedProvider(c.Provider, tracer)
		}

		// Wrap tools.
		if c.Tools != nil {
			c.Tools = WrapRegistry(c.Tools, tracer)
		}

		// Bridge metrics.
		if m, err := NewMetrics(cfg.meter()); err == nil {
			c.Metrics = m
		}
	}
}
