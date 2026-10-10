package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

// outcomeMetrics implements Metrics and both optional recorders.
type outcomeMetrics struct {
	types.NoopMetrics
	mu          sync.Mutex
	outcomes    []error
	invocations int
	cacheRead   int
	cacheWrite  int
}

func (m *outcomeMetrics) RecordAgentInvocation(context.Context, string, time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.invocations++
}

func (m *outcomeMetrics) RecordAgentOutcome(_ context.Context, _ string, _ time.Duration, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.outcomes = append(m.outcomes, err)
}

func (m *outcomeMetrics) RecordCacheTokenUsage(_ context.Context, _, _ string, read, write int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cacheRead += read
	m.cacheWrite += write
}

// recordingTracer counts runs and keeps the error each run ended with.
type recordingTracer struct {
	mu     sync.Mutex
	starts []string
	ends   []error
}

type tracedKey struct{}

func (r *recordingTracer) StartAgent(ctx context.Context, name string) (context.Context, func(error)) {
	r.mu.Lock()
	r.starts = append(r.starts, name)
	r.mu.Unlock()
	return context.WithValue(ctx, tracedKey{}, name), func(err error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.ends = append(r.ends, err)
	}
}

// ctxProvider records whether the run's context reached the provider.
type ctxProvider struct {
	*agenttest.ScriptedProvider
	traced bool
}

func (p *ctxProvider) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	msgs, tools := req.Messages, req.Tools
	_, p.traced = ctx.Value(tracedKey{}).(string)
	return p.ScriptedProvider.Stream(ctx, types.Request{Messages: msgs, Tools: tools})
}

func TestRunObservability(t *testing.T) {
	boom := errors.New("provider down")
	cached := usage(100, 5)
	cached.CachedPromptTokens, cached.CacheWriteTokens = 60, 20
	tests := []struct {
		name      string
		responses [][]types.Delta
		errs      []error
		wantErr   bool
		wantRead  int
	}{
		{name: "clean run with cache usage", responses: [][]types.Delta{withUsage(agenttest.TextResponse("ok"), cached)}, wantRead: 60},
		{name: "failed run", errs: []error{boom}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metrics := &outcomeMetrics{}
			tracer := &recordingTracer{}
			provider := &ctxProvider{ScriptedProvider: &agenttest.ScriptedProvider{Responses: tt.responses, Errors: tt.errs}}
			a := must.Get(New(Config{Name: "planner", Provider: provider, SystemPrompt: "sys", Metrics: metrics, RunTracer: tracer}))
			stream := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("hi"))})
			agenttest.CollectDeltas(stream.Deltas())
			runErr := stream.Wait()
			if (runErr != nil) != tt.wantErr {
				t.Fatalf("run err = %v, wantErr %v", runErr, tt.wantErr)
			}
			if len(tracer.starts) != 1 || tracer.starts[0] != "planner" || len(tracer.ends) != 1 {
				t.Fatalf("tracer starts %v ends %v", tracer.starts, tracer.ends)
			}
			if (tracer.ends[0] != nil) != tt.wantErr {
				t.Fatalf("span ended with %v", tracer.ends[0])
			}
			if !provider.traced {
				t.Fatal("the provider did not receive the traced context")
			}
			if metrics.invocations != 0 || len(metrics.outcomes) != 1 || (metrics.outcomes[0] != nil) != tt.wantErr {
				t.Fatalf("invocations %d outcomes %v", metrics.invocations, metrics.outcomes)
			}
			if metrics.cacheRead != tt.wantRead {
				t.Fatalf("cache read = %d, want %d", metrics.cacheRead, tt.wantRead)
			}
		})
	}
}
