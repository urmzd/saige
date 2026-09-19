package agent

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
)

// orderRecorder collects tool names in the order their bodies actually run,
// which is what an ordering-sensitive tool contract is graded on. The results
// slice the loop returns is indexed by request position either way, so it
// cannot distinguish the two execution modes.
type orderRecorder struct {
	mu    sync.Mutex
	order []string
}

func (r *orderRecorder) record(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.order = append(r.order, name)
}

func (r *orderRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.order)
}

// slowFirstTools returns three tools where the first one requested is by far
// the slowest. Run sequentially they record in request order; fanned out, the
// slow one lands last.
func slowFirstTools(r *orderRecorder, slow time.Duration) []types.Tool {
	mk := func(name string, d time.Duration) types.Tool {
		return &types.ToolFunc{
			Def: types.ToolDef{
				Name:       name,
				Parameters: types.ParameterSchema{Type: "object"},
			},
			Fn: func(context.Context, map[string]any) (string, error) {
				time.Sleep(d)
				r.record(name)
				return name, nil
			},
		}
	}
	return []types.Tool{mk("alpha", slow), mk("beta", 0), mk("gamma", 0)}
}

// threeCallTurn scripts one assistant turn requesting alpha, beta and gamma,
// then a closing text turn so the loop terminates.
func threeCallTurn() [][]types.Delta {
	return [][]types.Delta{
		{
			types.ToolCallStartDelta{ID: "tc-1", Name: "alpha"},
			types.ToolCallEndDelta{Arguments: map[string]any{}},
			types.ToolCallStartDelta{ID: "tc-2", Name: "beta"},
			types.ToolCallEndDelta{Arguments: map[string]any{}},
			types.ToolCallStartDelta{ID: "tc-3", Name: "gamma"},
			types.ToolCallEndDelta{Arguments: map[string]any{}},
		},
		agenttest.TextResponse("done"),
	}
}

func runTurn(t *testing.T, tools []types.Tool, opts ...AgentOption) {
	t.Helper()
	a := NewAgent(AgentConfig{
		Name:     "ordering",
		Provider: &agenttest.ScriptedProvider{Responses: threeCallTurn()},
		Tools:    types.NewToolRegistry(tools...),
	}, append([]AgentOption{WithMaxIter(5)}, opts...)...)

	stream := a.Invoke(context.Background(), []types.Message{types.NewUserMessage("go")})
	agenttest.CollectDeltas(stream.Deltas())
	stream.Wait()
}

func TestSequentialToolsPreservesRequestOrder(t *testing.T) {
	tests := []struct {
		name string
		opt  AgentOption
	}{
		{name: "WithSequentialTools", opt: WithSequentialTools()},
		{name: "WithMaxParallelTools(1)", opt: WithMaxParallelTools(1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &orderRecorder{}
			runTurn(t, slowFirstTools(r, 60*time.Millisecond), tt.opt)

			want := []string{"alpha", "beta", "gamma"}
			if got := r.snapshot(); !slices.Equal(got, want) {
				t.Errorf("execution order = %v, want %v", got, want)
			}
		})
	}
}

// Fan-out is still the default, and a cap above one still fans out, so neither
// may be read as a blanket "run sequentially" switch. Only alpha's position is
// asserted: the order the two instant tools reach the recorder is exactly the
// scheduler nondeterminism this option exists to remove.
func TestFannedOutToolsDoNotPreserveRequestOrder(t *testing.T) {
	tests := []struct {
		name string
		opts []AgentOption
	}{
		{name: "unlimited", opts: nil},
		{name: "cap above one", opts: []AgentOption{WithMaxParallelTools(3)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &orderRecorder{}
			runTurn(t, slowFirstTools(r, 60*time.Millisecond), tt.opts...)

			got := r.snapshot()
			if len(got) != 3 {
				t.Fatalf("recorded %d tools, want 3: %v", len(got), got)
			}
			if got[len(got)-1] != "alpha" {
				t.Errorf("execution order = %v, want the slow first-requested tool last", got)
			}
		})
	}
}
