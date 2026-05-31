package agent

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/urmzd/saige/agent/types"
)

// recordingRunner is an in-memory StepRunner that simulates DBOS replay: the
// first call for a step name runs fn and records the result; later calls with
// the same name return the recorded result WITHOUT running fn. Seeding a name
// up front simulates "this step completed before the crash."
type recordingRunner struct {
	mu       sync.Mutex
	recorded map[string]types.StepResult
	calls    map[string]int
}

func newRecordingRunner() *recordingRunner {
	return &recordingRunner{recorded: map[string]types.StepResult{}, calls: map[string]int{}}
}

func (r *recordingRunner) RunStep(ctx context.Context, name string, fn func(context.Context) (types.StepResult, error)) (types.StepResult, error) {
	r.mu.Lock()
	if res, ok := r.recorded[name]; ok {
		r.mu.Unlock()
		return res, nil // replay: do NOT run fn
	}
	r.mu.Unlock()

	res, err := fn(ctx)
	if err != nil {
		return res, err
	}
	r.mu.Lock()
	r.recorded[name] = res
	r.calls[name]++
	r.mu.Unlock()
	return res, nil
}

func (r *recordingRunner) seed(name string, res types.StepResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recorded[name] = res
}

func (r *recordingRunner) ranCount(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[name]
}

func (r *recordingRunner) has(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.recorded[name]
	return ok
}

// panicProvider panics if its ChatStream is ever called (used to prove an LLM
// step was served from a record, not re-executed).
type panicProvider struct{}

func (panicProvider) ChatStream(context.Context, []types.Message, []types.ToolDef) (<-chan types.Delta, error) {
	panic("provider must not be called on replay")
}

// countTool counts how many times Execute runs (proves tool-step memoization).
type countTool struct {
	name  string
	calls int32
}

func (t *countTool) Definition() types.ToolDef { return types.ToolDef{Name: t.name} }
func (t *countTool) Execute(context.Context, map[string]any) (string, error) {
	atomic.AddInt32(&t.calls, 1)
	return "fresh tool output", nil
}
func (t *countTool) count() int32 { return atomic.LoadInt32(&t.calls) }
