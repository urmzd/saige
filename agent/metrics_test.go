package agent

import (
	"context"
	"sync"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

// recordingMetrics captures RecordTokenUsage calls for assertions. It embeds
// NoopMetrics so the other Metrics methods are satisfied without ceremony.
type recordingMetrics struct {
	types.NoopMetrics
	mu     sync.Mutex
	tokens []tokenRecord
}

type tokenRecord struct {
	operation string
	provider  string
	input     int
	output    int
}

func (m *recordingMetrics) RecordTokenUsage(_ context.Context, operation, provider string, input, output int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens = append(m.tokens, tokenRecord{operation, provider, input, output})
}

func (m *recordingMetrics) tokenCalls() []tokenRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]tokenRecord, len(m.tokens))
	copy(out, m.tokens)
	return out
}

// usageProvider streams text plus a UsageDelta carrying prompt/completion
// counts, mimicking a real provider's usage reporting. Counts are split across
// two UsageDeltas to exercise the merge path the agent loop relies on.
type usageProvider struct {
	prompt     int
	completion int
}

func (p usageProvider) ChatStream(_ context.Context, _ []types.Message, _ []types.ToolDef) (<-chan types.Delta, error) {
	ch := make(chan types.Delta, 8)
	ch <- types.UsageDelta{PromptTokens: p.prompt} // message_start half
	ch <- types.TextStartDelta{}
	ch <- types.TextContentDelta{Content: "hi"}
	ch <- types.TextEndDelta{}
	ch <- types.UsageDelta{CompletionTokens: p.completion} // message_delta half
	close(ch)
	return ch, nil
}

// noUsageProvider streams text but never reports usage.
type noUsageProvider struct{}

func (noUsageProvider) ChatStream(_ context.Context, _ []types.Message, _ []types.ToolDef) (<-chan types.Delta, error) {
	ch := make(chan types.Delta, 3)
	ch <- types.TextStartDelta{}
	ch <- types.TextContentDelta{Content: "hi"}
	ch <- types.TextEndDelta{}
	close(ch)
	return ch, nil
}

func TestRecordTokenUsage(t *testing.T) {
	tests := []struct {
		name      string
		provider  types.Provider
		wantCalls int
		wantInput int
		wantOut   int
	}{
		{
			name:      "merged prompt and completion tokens recorded once",
			provider:  usageProvider{prompt: 100, completion: 42},
			wantCalls: 1,
			wantInput: 100,
			wantOut:   42,
		},
		{
			name:      "no usage reported records nothing",
			provider:  noUsageProvider{},
			wantCalls: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recordingMetrics{}
			a := NewAgent(AgentConfig{
				Provider:     tt.provider,
				SystemPrompt: "s",
				Metrics:      rec,
			})

			deltas := collectDeltas(a.Invoke(context.Background(), []types.Message{types.NewUserMessage("hi")}))
			if errs := collectDeltasByType[types.ErrorDelta](deltas); len(errs) != 0 {
				t.Fatalf("unexpected error deltas: %v", errs)
			}

			calls := rec.tokenCalls()
			if len(calls) != tt.wantCalls {
				t.Fatalf("RecordTokenUsage fired %d times, want %d (%+v)", len(calls), tt.wantCalls, calls)
			}
			if tt.wantCalls == 0 {
				return
			}
			got := calls[0]
			if got.input != tt.wantInput || got.output != tt.wantOut {
				t.Errorf("token counts = (in %d, out %d), want (in %d, out %d)",
					got.input, got.output, tt.wantInput, tt.wantOut)
			}
			if got.operation != "chat" {
				t.Errorf("operation = %q, want %q", got.operation, "chat")
			}
			if got.provider == "" {
				t.Error("provider name should not be empty")
			}
		})
	}
}

// TestRecordTokenUsage_CacheHitSkipped verifies a cache-hit usage delta does not
// record fresh token usage (no new tokens were produced).
func TestRecordTokenUsage_CacheHitSkipped(t *testing.T) {
	rec := &recordingMetrics{}
	a := NewAgent(AgentConfig{
		Provider:     cacheHitProvider{prompt: 100, completion: 42},
		SystemPrompt: "s",
		Metrics:      rec,
	})

	_ = collectDeltas(a.Invoke(context.Background(), []types.Message{types.NewUserMessage("hi")}))
	if calls := rec.tokenCalls(); len(calls) != 0 {
		t.Errorf("RecordTokenUsage fired %d times on a cache hit, want 0 (%+v)", len(calls), calls)
	}
}

// cacheHitProvider reports usage flagged as a cache hit.
type cacheHitProvider struct {
	prompt     int
	completion int
}

func (p cacheHitProvider) ChatStream(_ context.Context, _ []types.Message, _ []types.ToolDef) (<-chan types.Delta, error) {
	ch := make(chan types.Delta, 4)
	ch <- types.TextStartDelta{}
	ch <- types.TextContentDelta{Content: "hi"}
	ch <- types.TextEndDelta{}
	ch <- types.UsageDelta{PromptTokens: p.prompt, CompletionTokens: p.completion, CacheHit: true}
	close(ch)
	return ch, nil
}
