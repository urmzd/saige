package otel

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/urmzd/saige/agent/types"
)

var _ types.Metrics = (*Metrics)(nil)

// Metrics implements types.Metrics using OpenTelemetry metrics.
type Metrics struct {
	tokenUsage    metric.Int64Counter
	toolDuration  metric.Float64Histogram
	llmDuration   metric.Float64Histogram
	agentDuration metric.Float64Histogram
}

// NewMetrics creates an OTel-backed Metrics implementation.
func NewMetrics(meter metric.Meter) (*Metrics, error) {
	tokenUsage, err := meter.Int64Counter("gen_ai.token_usage",
		metric.WithDescription("Token usage by direction"),
	)
	if err != nil {
		return nil, err
	}

	toolDuration, err := meter.Float64Histogram("tool.duration",
		metric.WithDescription("Tool execution duration in seconds"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}

	llmDuration, err := meter.Float64Histogram("gen_ai.duration",
		metric.WithDescription("LLM provider call duration in seconds"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}

	agentDuration, err := meter.Float64Histogram("agent.duration",
		metric.WithDescription("Agent invocation duration in seconds"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}

	return &Metrics{
		tokenUsage:    tokenUsage,
		toolDuration:  toolDuration,
		llmDuration:   llmDuration,
		agentDuration: agentDuration,
	}, nil
}

func (m *Metrics) RecordTokenUsage(ctx context.Context, input, output int) {
	m.tokenUsage.Add(ctx, int64(input), metric.WithAttributes(
		attribute.String("direction", "input"),
	))
	m.tokenUsage.Add(ctx, int64(output), metric.WithAttributes(
		attribute.String("direction", "output"),
	))
}

func (m *Metrics) RecordToolCall(ctx context.Context, toolName string, duration time.Duration, err error) {
	attrs := []attribute.KeyValue{
		attribute.String("tool.name", toolName),
		attribute.Bool("error", err != nil),
	}
	m.toolDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(attrs...))
}

func (m *Metrics) RecordProviderCall(ctx context.Context, provider string, duration time.Duration, err error) {
	attrs := []attribute.KeyValue{
		attribute.String("gen_ai.system", provider),
		attribute.Bool("error", err != nil),
	}
	m.llmDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(attrs...))
}

func (m *Metrics) RecordAgentInvocation(ctx context.Context, agentID string, duration time.Duration) {
	attrs := []attribute.KeyValue{
		attribute.String("agent.name", agentID),
	}
	m.agentDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(attrs...))
}
