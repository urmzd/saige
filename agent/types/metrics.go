package types

import (
	"context"
	"time"
)

// Metrics collects operational telemetry from the agent loop.
type Metrics interface {
	RecordTokenUsage(ctx context.Context, operationName, provider string, input, output int)
	RecordToolCall(ctx context.Context, toolName string, duration time.Duration, err error)
	RecordProviderCall(ctx context.Context, operationName, provider string, duration time.Duration, err error)
	RecordAgentInvocation(ctx context.Context, agentID string, duration time.Duration)
}

// AgentOutcomeRecorder is an optional Metrics extension. The agent calls
// RecordAgentOutcome in place of RecordAgentInvocation, with the run's
// terminal error (nil on a clean finish).
type AgentOutcomeRecorder interface {
	RecordAgentOutcome(ctx context.Context, agentID string, duration time.Duration, err error)
}

// CacheUsageRecorder is an optional Metrics extension that receives the
// prompt-cache token counts of each provider call: tokens read from the
// cache and tokens written to it.
type CacheUsageRecorder interface {
	RecordCacheTokenUsage(ctx context.Context, operationName, provider string, cacheRead, cacheWrite int)
}

// NoopMetrics is a no-op implementation of Metrics.
type NoopMetrics struct{}

// RecordTokenUsage implements Metrics.
func (NoopMetrics) RecordTokenUsage(context.Context, string, string, int, int) {}

// RecordToolCall implements Metrics.
func (NoopMetrics) RecordToolCall(context.Context, string, time.Duration, error) {}

// RecordProviderCall implements Metrics.
func (NoopMetrics) RecordProviderCall(context.Context, string, string, time.Duration, error) {}

// RecordAgentInvocation implements Metrics.
func (NoopMetrics) RecordAgentInvocation(context.Context, string, time.Duration) {}
