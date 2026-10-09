package otel

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/types"
)

// Config configures OpenTelemetry tracing and metrics for SAIGE agents.
type Config struct {
	// TracerProvider to use. If nil, the global provider is used.
	TracerProvider trace.TracerProvider
	// MeterProvider to use for metrics. If nil, the global provider is used.
	MeterProvider metric.MeterProvider
	// ServiceName used for the tracer and meter. Defaults to "saige".
	ServiceName string
	// Redactor, when set, rewrites every error message before it is written to
	// a span status or exception event. Provider and tool errors can embed
	// request bodies or user data; use this to mask them before they reach the
	// telemetry backend.
	Redactor func(string) string
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
//
// Tools are wrapped with WrapTool, so marked tools still prompt for approval,
// rich results keep their blocks and citations, and handoff and sub-agent
// tools keep transferring control. Sub-agent and handoff members already
// configured when the option runs get the same treatment for their own
// provider and tools; place WithSubAgents and WithHandoffs before WithTracing,
// or set them on the base AgentConfig.
//
// A Metrics sink set before this option keeps receiving every record
// alongside the OTel bridge. A meter that fails to create its instruments is
// logged and leaves the existing sink in place.
func WithTracing(cfg Config) agent.AgentOption {
	return func(c *agent.AgentConfig) {
		tracer := cfg.tracer()

		m, err := NewMetrics(cfg.meter())
		if err != nil {
			logger := c.Logger
			if logger == nil {
				logger = slog.Default()
			}
			logger.Error("otel: metrics disabled, instrument creation failed", "error", err)
			m = nil
		}

		popts := []ProviderOption{WithProviderRedactor(cfg.Redactor)}
		if m != nil {
			popts = append(popts, WithProviderMetrics(m))
		}
		topts := []ToolOption{WithToolRedactor(cfg.Redactor)}

		c.Provider = traceProvider(c.Provider, tracer, popts)
		if c.Tools != nil {
			c.Tools = WrapRegistry(c.Tools, tracer, topts...)
		}
		// Copy before editing so a caller's slice of definitions is not changed.
		c.SubAgents = append([]agent.SubAgentDef(nil), c.SubAgents...)
		for i := range c.SubAgents {
			c.SubAgents[i].Provider = traceProvider(c.SubAgents[i].Provider, tracer, popts)
			if c.SubAgents[i].Tools != nil {
				c.SubAgents[i].Tools = WrapRegistry(c.SubAgents[i].Tools, tracer, topts...)
			}
		}
		c.Handoffs = append([]agent.HandoffDef(nil), c.Handoffs...)
		for i := range c.Handoffs {
			c.Handoffs[i].Provider = traceProvider(c.Handoffs[i].Provider, tracer, popts)
			if c.Handoffs[i].Tools != nil {
				c.Handoffs[i].Tools = WrapRegistry(c.Handoffs[i].Tools, tracer, topts...)
			}
		}

		if m != nil {
			c.Metrics = combineMetrics(c.Metrics, m)
		}
		if c.RunTracer == nil {
			c.RunTracer = NewAgentTracer(cfg)
		}
	}
}

// traceProvider wraps p once. A nil provider stays nil so a member that
// inherits its parent's provider keeps doing so.
func traceProvider(p types.Provider, tracer trace.Tracer, opts []ProviderOption) types.Provider {
	if p == nil {
		return nil
	}
	if _, ok := p.(*TracedProvider); ok {
		return p
	}
	return NewTracedProvider(p, tracer, opts...)
}

// combineMetrics returns a sink that records to both existing and m. A nil or
// no-op existing sink, or an earlier OTel bridge, is replaced by m. A fan-out
// built by an earlier call keeps its other members and has its OTel bridge
// replaced, so applying WithTracing twice still records each call once.
func combineMetrics(existing types.Metrics, m *Metrics) types.Metrics {
	switch v := existing.(type) {
	case nil, types.NoopMetrics, *types.NoopMetrics, *Metrics:
		return m
	case fanoutMetrics:
		out := make(fanoutMetrics, 0, len(v)+1)
		for _, member := range v {
			if _, ok := member.(*Metrics); !ok {
				out = append(out, member)
			}
		}
		if len(out) == 0 {
			return m
		}
		return append(out, m)
	}
	return fanoutMetrics{existing, m}
}

// fanoutMetrics delivers every record to each member in order.
type fanoutMetrics []types.Metrics

func (f fanoutMetrics) RecordTokenUsage(ctx context.Context, operationName, provider string, input, output int) {
	for _, m := range f {
		m.RecordTokenUsage(ctx, operationName, provider, input, output)
	}
}

func (f fanoutMetrics) RecordToolCall(ctx context.Context, toolName string, duration time.Duration, err error) {
	for _, m := range f {
		m.RecordToolCall(ctx, toolName, duration, err)
	}
}

func (f fanoutMetrics) RecordProviderCall(ctx context.Context, operationName, provider string, duration time.Duration, err error) {
	for _, m := range f {
		m.RecordProviderCall(ctx, operationName, provider, duration, err)
	}
}

func (f fanoutMetrics) RecordAgentInvocation(ctx context.Context, agentID string, duration time.Duration) {
	for _, m := range f {
		m.RecordAgentInvocation(ctx, agentID, duration)
	}
}

// RecordAgentOutcome forwards the outcome to members that accept it and the
// plain invocation to those that do not.
func (f fanoutMetrics) RecordAgentOutcome(ctx context.Context, agentID string, duration time.Duration, err error) {
	for _, m := range f {
		if r, ok := m.(interface {
			RecordAgentOutcome(context.Context, string, time.Duration, error)
		}); ok {
			r.RecordAgentOutcome(ctx, agentID, duration, err)
			continue
		}
		m.RecordAgentInvocation(ctx, agentID, duration)
	}
}

// RecordCacheTokenUsage forwards cache token counts to members that accept
// them.
func (f fanoutMetrics) RecordCacheTokenUsage(ctx context.Context, operationName, provider string, cacheRead, cacheWrite int) {
	for _, m := range f {
		if r, ok := m.(interface {
			RecordCacheTokenUsage(context.Context, string, string, int, int)
		}); ok {
			r.RecordCacheTokenUsage(ctx, operationName, provider, cacheRead, cacheWrite)
		}
	}
}

// AgentTracer opens one invoke_agent span per agent run, so the chat and
// execute_tool spans of the run, and the runs of any sub-agents it delegates
// to, share one trace under it.
type AgentTracer struct {
	tracer   trace.Tracer
	redactor func(string) string
}

// NewAgentTracer returns an AgentTracer for cfg.
func NewAgentTracer(cfg Config) *AgentTracer {
	return &AgentTracer{tracer: cfg.tracer(), redactor: cfg.Redactor}
}

// StartAgent opens an "invoke_agent {name}" span and returns a context
// carrying it. Pass that context to the run so its provider and tool spans
// become children. end closes the span and marks it failed when err is
// non-nil; calls after the first are ignored.
func (t *AgentTracer) StartAgent(ctx context.Context, name string) (context.Context, func(err error)) {
	spanName := "invoke_agent"
	attrs := []attribute.KeyValue{attribute.String("gen_ai.operation.name", "invoke_agent")}
	if name != "" {
		spanName += " " + name
		attrs = append(attrs, attribute.String("gen_ai.agent.name", name))
	}
	ctx, span := t.tracer.Start(ctx, spanName,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attrs...),
	)
	var once sync.Once
	return ctx, func(err error) {
		once.Do(func() {
			recordSpanError(span, err, t.redactor)
			span.End()
		})
	}
}
