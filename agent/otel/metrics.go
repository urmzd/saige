package otel

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/urmzd/saige/agent/types"
)

var _ types.Metrics = (*Metrics)(nil)

// Metrics implements types.Metrics using OpenTelemetry metrics,
// following the GenAI semantic conventions.
//
// Per the GenAI conventions, gen_ai.client.operation.duration is a single
// instrument disambiguated by the gen_ai.operation.name attribute (chat,
// execute_tool, invoke_agent). Registering it under one name avoids duplicate
// instrument warnings and double-counted histograms; the operation is keyed by
// attribute, not by a separate instrument.
type Metrics struct {
	tokenUsage        metric.Int64Histogram
	operationDuration metric.Float64Histogram
}

// NewMetrics creates an OTel-backed Metrics implementation.
func NewMetrics(meter metric.Meter) (*Metrics, error) {
	tokenUsage, err := meter.Int64Histogram("gen_ai.client.token.usage",
		metric.WithDescription("Measures number of input and output tokens used"),
		metric.WithUnit("{token}"),
	)
	if err != nil {
		return nil, err
	}

	operationDuration, err := meter.Float64Histogram("gen_ai.client.operation.duration",
		metric.WithDescription("GenAI operation duration"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}

	return &Metrics{
		tokenUsage:        tokenUsage,
		operationDuration: operationDuration,
	}, nil
}

func (m *Metrics) RecordTokenUsage(ctx context.Context, operationName, provider string, input, output int) {
	baseAttrs := []attribute.KeyValue{
		attribute.String("gen_ai.operation.name", operationName),
		attribute.String("gen_ai.provider.name", provider),
	}

	inputAttrs := append([]attribute.KeyValue{
		attribute.String("gen_ai.token.type", "input"),
	}, baseAttrs...)
	m.tokenUsage.Record(ctx, int64(input), metric.WithAttributes(inputAttrs...))

	outputAttrs := append([]attribute.KeyValue{
		attribute.String("gen_ai.token.type", "output"),
	}, baseAttrs...)
	m.tokenUsage.Record(ctx, int64(output), metric.WithAttributes(outputAttrs...))
}

func (m *Metrics) RecordToolCall(ctx context.Context, toolName string, duration time.Duration, err error) {
	attrs := []attribute.KeyValue{
		attribute.String("gen_ai.operation.name", "execute_tool"),
		attribute.String("gen_ai.tool.name", toolName),
	}
	if err != nil {
		attrs = append(attrs, attribute.String("error.type", errorType(err)))
	}
	m.operationDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(attrs...))
}

func (m *Metrics) RecordProviderCall(ctx context.Context, operationName, provider string, duration time.Duration, err error) {
	attrs := []attribute.KeyValue{
		attribute.String("gen_ai.operation.name", operationName),
		attribute.String("gen_ai.provider.name", provider),
	}
	if err != nil {
		attrs = append(attrs, attribute.String("error.type", errorType(err)))
	}
	m.operationDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(attrs...))
}

func (m *Metrics) RecordAgentInvocation(ctx context.Context, agentID string, duration time.Duration) {
	attrs := []attribute.KeyValue{
		attribute.String("gen_ai.operation.name", "invoke_agent"),
		attribute.String("gen_ai.agent.name", agentID),
	}
	m.operationDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(attrs...))
}
