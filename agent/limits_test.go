package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// ===================================================================
// (a) Per-call timeouts
// ===================================================================

// hangingProvider blocks inside ChatStream until ctx is cancelled, then closes
// the channel without emitting any delta. A well-behaved provider that observes
// the deadline behaves exactly like this.
type hangingProvider struct {
	started chan struct{}
	once    sync.Once
}

func (p *hangingProvider) ChatStream(ctx context.Context, _ []types.Message, _ []types.ToolDef) (<-chan types.Delta, error) {
	ch := make(chan types.Delta)
	go func() {
		defer close(ch)
		if p.started != nil {
			p.once.Do(func() { close(p.started) })
		}
		<-ctx.Done() // honour the (timeout) context
	}()
	return ch, nil
}

func TestLLMTimeoutSurfacesError(t *testing.T) {
	provider := &hangingProvider{started: make(chan struct{})}

	agent := NewAgent(AgentConfig{
		Provider:     provider,
		SystemPrompt: "sys",
	}, WithLLMTimeout(50*time.Millisecond))

	start := time.Now()
	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("hi")})
	deltas := collectDeltas(stream)
	stream.Wait()
	elapsed := time.Since(start)

	// The call must be cancelled, not hang.
	if elapsed > 2*time.Second {
		t.Fatalf("loop took %s, expected timeout to fire quickly", elapsed)
	}

	errDeltas := collectDeltasByType[types.ErrorDelta](deltas)
	if len(errDeltas) != 1 {
		t.Fatalf("ErrorDelta count = %d, want 1", len(errDeltas))
	}
	if !errors.Is(errDeltas[0].Error, context.DeadlineExceeded) {
		t.Errorf("error = %v, want wrapped DeadlineExceeded", errDeltas[0].Error)
	}
	if !errors.Is(errDeltas[0].Error, types.ErrProviderFailed) {
		t.Errorf("error = %v, want ErrProviderFailed", errDeltas[0].Error)
	}
	if !types.IsTransient(errDeltas[0].Error) {
		t.Errorf("timeout error should be classified transient, got %v", errDeltas[0].Error)
	}
}

func TestLLMTimeoutDisabledByDefault(t *testing.T) {
	// With no timeout configured, a normal fast provider completes cleanly.
	provider := &mockProvider{response: "hello"}
	agent := NewAgent(AgentConfig{Provider: provider, SystemPrompt: "sys"})

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("hi")})
	deltas := collectDeltas(stream)
	if err := stream.Wait(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if agent.cfg.LLMTimeout != 0 {
		t.Errorf("default LLMTimeout = %v, want 0", agent.cfg.LLMTimeout)
	}
	if errs := collectDeltasByType[types.ErrorDelta](deltas); len(errs) != 0 {
		t.Errorf("unexpected error deltas: %v", errs)
	}
	if txt := textFromDeltas(deltas); txt != "hello" {
		t.Errorf("text = %q, want 'hello'", txt)
	}
}

// hangingTool blocks until ctx is cancelled, then returns the deadline error.
type hangingTool struct {
	name string
}

func (t *hangingTool) Definition() types.ToolDef {
	return types.ToolDef{Name: t.name, Description: "hangs"}
}

func (t *hangingTool) Execute(ctx context.Context, _ map[string]any) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

func TestToolTimeoutSurfacesError(t *testing.T) {
	provider := &toolCallProvider{
		toolName: "hang",
		toolID:   "call-1",
		toolArgs: map[string]any{},
		response: "done after tool",
	}

	agent := NewAgent(AgentConfig{
		Provider:     provider,
		SystemPrompt: "sys",
		Tools:        types.NewToolRegistry(&hangingTool{name: "hang"}),
	}, WithToolTimeout(50*time.Millisecond))

	start := time.Now()
	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("run it")})
	deltas := collectDeltas(stream)
	stream.Wait()
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("tool took %s, expected timeout to fire quickly", elapsed)
	}

	execEnds := collectDeltasByType[types.ToolExecEndDelta](deltas)
	if len(execEnds) != 1 {
		t.Fatalf("ToolExecEndDelta count = %d, want 1", len(execEnds))
	}
	if execEnds[0].Error == "" {
		t.Fatalf("expected a tool error from timeout, got none")
	}
	if !strings.Contains(execEnds[0].Error, "context deadline exceeded") {
		t.Errorf("tool error = %q, want deadline exceeded", execEnds[0].Error)
	}
}

// ignoresCtxSlowTool ignores stepCtx and always sleeps past the deadline; its
// completion-after-deadline must still be reported as a timeout error.
type ignoresCtxSlowTool struct {
	name  string
	sleep time.Duration
}

func (t *ignoresCtxSlowTool) Definition() types.ToolDef {
	return types.ToolDef{Name: t.name, Description: "slow"}
}

func (t *ignoresCtxSlowTool) Execute(_ context.Context, _ map[string]any) (string, error) {
	time.Sleep(t.sleep)
	return "late result", nil
}

func TestToolTimeoutCatchesDeadlineIgnoringTool(t *testing.T) {
	provider := &toolCallProvider{
		toolName: "slow",
		toolID:   "call-1",
		toolArgs: map[string]any{},
		response: "done",
	}

	agent := NewAgent(AgentConfig{
		Provider:     provider,
		SystemPrompt: "sys",
		Tools:        types.NewToolRegistry(&ignoresCtxSlowTool{name: "slow", sleep: 30 * time.Millisecond}),
	}, WithToolTimeout(5*time.Millisecond))

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("run it")})
	deltas := collectDeltas(stream)
	stream.Wait()

	execEnds := collectDeltasByType[types.ToolExecEndDelta](deltas)
	if len(execEnds) != 1 {
		t.Fatalf("ToolExecEndDelta count = %d, want 1", len(execEnds))
	}
	if !strings.Contains(execEnds[0].Error, "context deadline exceeded") {
		t.Errorf("expected deadline-exceeded error, got %q (result %q)", execEnds[0].Error, execEnds[0].Result)
	}
	if execEnds[0].Result != "" {
		t.Errorf("result = %q, want empty on timeout", execEnds[0].Result)
	}
}

func TestToolTimeoutFastToolUnaffected(t *testing.T) {
	called := 0
	tool := &types.ToolFunc{
		Def: types.ToolDef{Name: "fast", Description: "fast"},
		Fn: func(_ context.Context, _ map[string]any) (string, error) {
			called++
			return "fast result", nil
		},
	}
	provider := &toolCallProvider{toolName: "fast", toolID: "c1", toolArgs: map[string]any{}, response: "ok"}

	agent := NewAgent(AgentConfig{
		Provider:     provider,
		SystemPrompt: "sys",
		Tools:        types.NewToolRegistry(tool),
	}, WithToolTimeout(time.Second))

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("go")})
	deltas := collectDeltas(stream)
	stream.Wait()

	execEnds := collectDeltasByType[types.ToolExecEndDelta](deltas)
	if len(execEnds) != 1 || execEnds[0].Error != "" {
		t.Fatalf("expected one clean tool exec, got %+v", execEnds)
	}
	if execEnds[0].Result != "fast result" {
		t.Errorf("result = %q, want 'fast result'", execEnds[0].Result)
	}
}

// ===================================================================
// (b) Parallel-tool concurrency cap
// ===================================================================

// concurrencyProbeTool tracks the maximum number of concurrent executions seen.
type concurrencyProbeTool struct {
	name        string
	hold        time.Duration
	mu          sync.Mutex
	current     int
	maxObserved int
}

func (t *concurrencyProbeTool) Definition() types.ToolDef {
	return types.ToolDef{Name: t.name, Description: "probe"}
}

func (t *concurrencyProbeTool) Execute(_ context.Context, _ map[string]any) (string, error) {
	t.mu.Lock()
	t.current++
	if t.current > t.maxObserved {
		t.maxObserved = t.current
	}
	t.mu.Unlock()

	time.Sleep(t.hold) // overlap window

	t.mu.Lock()
	t.current--
	t.mu.Unlock()
	return "ok", nil
}

func (t *concurrencyProbeTool) maxConcurrent() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.maxObserved
}

// manyToolCallProvider issues n distinct calls to the same tool in one turn,
// then returns text.
func manyToolCallProvider(toolName string, n int) *multiToolCallProvider {
	p := &multiToolCallProvider{response: "all done"}
	for i := 0; i < n; i++ {
		p.toolCalls = append(p.toolCalls, struct {
			ID   string
			Name string
			Args map[string]any
		}{ID: fmt.Sprintf("c%d", i), Name: toolName, Args: map[string]any{}})
	}
	return p
}

func TestMaxParallelToolsCapsConcurrency(t *testing.T) {
	const cap = 2
	probe := &concurrencyProbeTool{name: "probe", hold: 40 * time.Millisecond}
	provider := manyToolCallProvider("probe", 6)

	agent := NewAgent(AgentConfig{
		Provider:     provider,
		SystemPrompt: "sys",
		Tools:        types.NewToolRegistry(probe),
	}, WithMaxParallelTools(cap))

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("fan out")})
	collectDeltas(stream)
	stream.Wait()

	if got := probe.maxConcurrent(); got > cap {
		t.Errorf("observed %d concurrent tool executions, cap was %d", got, cap)
	}
	if got := probe.maxConcurrent(); got == 0 {
		t.Error("probe never observed any execution")
	}
}

func TestUnlimitedToolsRunFullyParallel(t *testing.T) {
	const n = 5
	probe := &concurrencyProbeTool{name: "probe", hold: 40 * time.Millisecond}
	provider := manyToolCallProvider("probe", n)

	// MaxParallelTools = 0 (unlimited): all n should overlap.
	agent := NewAgent(AgentConfig{
		Provider:     provider,
		SystemPrompt: "sys",
		Tools:        types.NewToolRegistry(probe),
	})

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("fan out")})
	collectDeltas(stream)
	stream.Wait()

	if got := probe.maxConcurrent(); got != n {
		t.Errorf("max concurrency = %d, want %d (unlimited)", got, n)
	}
}

func TestMaxParallelToolsAllResultsReturned(t *testing.T) {
	probe := &concurrencyProbeTool{name: "probe", hold: 5 * time.Millisecond}
	provider := manyToolCallProvider("probe", 4)

	agent := NewAgent(AgentConfig{
		Provider:     provider,
		SystemPrompt: "sys",
		Tools:        types.NewToolRegistry(probe),
	}, WithMaxParallelTools(1)) // fully serial, still correct

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("fan out")})
	deltas := collectDeltas(stream)
	stream.Wait()

	execEnds := collectDeltasByType[types.ToolExecEndDelta](deltas)
	if len(execEnds) != 4 {
		t.Errorf("ToolExecEndDelta count = %d, want 4 (all tools ran)", len(execEnds))
	}
	if got := probe.maxConcurrent(); got != 1 {
		t.Errorf("cap=1 should serialize, observed %d concurrent", got)
	}
}

// ===================================================================
// (c) Max-iteration truncation signal
// ===================================================================

// infiniteToolProvider always asks to call a tool, never finishing with text.
func infiniteToolProvider(toolName string) *sequenceProvider {
	p := &sequenceProvider{responses: make([]func(ch chan<- types.Delta), 100)}
	for i := range p.responses {
		i := i
		p.responses[i] = func(ch chan<- types.Delta) {
			ch <- types.ToolCallStartDelta{ID: fmt.Sprintf("call-%d", i), Name: toolName}
			ch <- types.ToolCallEndDelta{Arguments: map[string]any{}}
		}
	}
	return p
}

func TestMaxIterationsEmitsErrMaxIterationsWhenTruncated(t *testing.T) {
	var calls int32
	tool := &types.ToolFunc{
		Def: types.ToolDef{Name: "loop", Description: "loop"},
		Fn: func(_ context.Context, _ map[string]any) (string, error) {
			atomic.AddInt32(&calls, 1)
			return "ok", nil
		},
	}

	agent := NewAgent(AgentConfig{
		Provider:     infiniteToolProvider("loop"),
		SystemPrompt: "sys",
		Tools:        types.NewToolRegistry(tool),
		MaxIter:      3,
	})

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("loop")})
	deltas := collectDeltas(stream)
	stream.Wait()

	errDeltas := collectDeltasByType[types.ErrorDelta](deltas)
	found := false
	for _, e := range errDeltas {
		if errors.Is(e.Error, types.ErrMaxIterations) {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected ErrMaxIterations delta on truncated run, got error deltas: %v", errDeltas)
	}

	// And the run still terminates (DoneDelta present).
	if dones := collectDeltasByType[types.DoneDelta](deltas); len(dones) != 1 {
		t.Errorf("DoneDelta count = %d, want 1", len(dones))
	}
}

func TestCleanFinishDoesNotEmitErrMaxIterations(t *testing.T) {
	// Provider responds with text immediately -> natural finish, well within cap.
	agent := NewAgent(AgentConfig{
		Provider:     &mockProvider{response: "all done"},
		SystemPrompt: "sys",
		MaxIter:      5,
	})

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("hi")})
	deltas := collectDeltas(stream)
	stream.Wait()

	for _, e := range collectDeltasByType[types.ErrorDelta](deltas) {
		if errors.Is(e.Error, types.ErrMaxIterations) {
			t.Fatalf("ErrMaxIterations emitted on a clean finish")
		}
	}
}

func TestMaxIterationsCleanFinishOnLastTurnNoError(t *testing.T) {
	// A tool turn followed by a text turn that lands exactly on the cap is a
	// clean finish: the assistant consumed the tool results, so no truncation.
	tool := &types.ToolFunc{
		Def: types.ToolDef{Name: "step", Description: "step"},
		Fn:  func(_ context.Context, _ map[string]any) (string, error) { return "ok", nil },
	}
	// iter 0: tool call, iter 1: text finish. MaxIter=2 lets both run.
	provider := &multiTurnToolProvider{toolTurns: 1, toolName: "step", finalMessage: "finished cleanly"}

	agent := NewAgent(AgentConfig{
		Provider:     provider,
		SystemPrompt: "sys",
		Tools:        types.NewToolRegistry(tool),
		MaxIter:      2,
	})

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("go")})
	deltas := collectDeltas(stream)
	stream.Wait()

	if txt := textFromDeltas(deltas); txt != "finished cleanly" {
		t.Errorf("final text = %q, want 'finished cleanly'", txt)
	}
	for _, e := range collectDeltasByType[types.ErrorDelta](deltas) {
		if errors.Is(e.Error, types.ErrMaxIterations) {
			t.Fatal("ErrMaxIterations emitted even though the assistant finished cleanly")
		}
	}
}
