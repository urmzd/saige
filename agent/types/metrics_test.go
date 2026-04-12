package types

import (
	"context"
	"testing"
	"time"
)

func TestNoopMetrics(t *testing.T) {
	m := NoopMetrics{}
	ctx := context.Background()

	// Should not panic
	m.RecordTokenUsage(ctx, "chat", "ollama", 100, 50)
	m.RecordToolCall(ctx, "test", time.Second, nil)
	m.RecordProviderCall(ctx, "chat", "ollama", time.Second, nil)
	m.RecordAgentInvocation(ctx, "agent-1", time.Second)
}
