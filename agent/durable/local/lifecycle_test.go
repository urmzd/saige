package local

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

func TestRunRecordsSetupFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		factory Factory
		seed    []types.BudgetReceipt
	}{
		{name: "nil agent", factory: func() *agent.Agent { return nil }},
		{
			name: "budget restore fails",
			factory: func() *agent.Agent {
				return must.Get(agent.New(agent.Config{Provider: &agenttest.ScriptedProvider{}, Budget: types.NewBudget(types.BudgetPolicy{MaxRequests: 1})}))
			},
			seed: []types.BudgetReceipt{{ID: "dup", Usage: types.TokenUsage{Requests: 1}}, {ID: "dup", Usage: types.TokenUsage{Requests: 1}}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := New(t.TempDir())
			input := []types.Message{types.UserMsg(types.Text("go"))}
			if tc.seed != nil {
				raw, err := encode(input)
				if err != nil {
					t.Fatal(err)
				}
				path, release, err := e.acquire("run")
				if err != nil {
					t.Fatal(err)
				}
				r := &runner{path: path, state: State{Version: 1, RunID: "run", Revision: "v1", Status: statusFailed, Input: raw, Steps: map[string]Step{}, Interrupts: map[string]Interrupt{}, ReconciledReceipts: tc.seed}}
				if err := r.save(); err != nil {
					t.Fatal(err)
				}
				release()
			}
			if _, err := e.Run(context.Background(), "run", "v1", tc.factory, input); err == nil {
				t.Fatal("Run succeeded with a broken factory")
			}
			state, err := e.Inspect("run")
			if err != nil {
				t.Fatal(err)
			}
			if state.Status != statusFailed || state.Error == "" {
				t.Errorf("status=%q error=%q, want failed with an error", state.Status, state.Error)
			}
		})
	}
}

func TestLeased(t *testing.T) {
	e := New(t.TempDir())
	if _, err := e.Leased("never"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unknown run err = %v, want os.ErrNotExist", err)
	}
	path, release, err := e.acquire("run")
	if err != nil {
		t.Fatal(err)
	}
	r := &runner{path: path, state: State{Version: 1, RunID: "run", Revision: "v1", Status: statusRunning, Steps: map[string]Step{}, Interrupts: map[string]Interrupt{}}}
	if err := r.save(); err != nil {
		t.Fatal(err)
	}
	if leased, err := e.Leased("run"); err != nil || !leased {
		t.Errorf("held lease: leased=%v err=%v, want true", leased, err)
	}
	release()
	if leased, err := e.Leased("run"); err != nil || leased {
		t.Errorf("released lease: leased=%v err=%v, want false", leased, err)
	}
}

// TestListAndDelete creates completed, suspended and cancelled runs, lists
// them, and deletes the ones that are finished.
func TestListAndDelete(t *testing.T) {
	ctx := context.Background()
	e := New(t.TempDir())
	var calls atomic.Int32
	input := []types.Message{types.UserMsg(types.Text("go"))}

	plain := func() *agent.Agent {
		return must.Get(agent.New(agent.Config{Provider: &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("done")}}}))
	}
	approval := func() *agent.Agent {
		write := &types.ToolFunc{Def: types.ToolDef{Name: "write"}, Fn: func(context.Context, map[string]any) (string, error) { return "ok", nil }}
		read := &types.ToolFunc{Def: types.ToolDef{Name: "read"}, Fn: func(context.Context, map[string]any) (string, error) { return "ok", nil }}
		return must.Get(agent.New(agent.Config{Provider: provider{&calls}, Tools: types.NewToolRegistry(types.WithMarkers(write, types.Marker{Kind: "approval"}), read), MaxParallelTools: 1}))
	}
	if _, err := e.Run(ctx, "completed", "v1", plain, input); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Run(ctx, "suspended", "v1", approval, input); !errors.Is(err, types.ErrSuspended) {
		t.Fatal(err)
	}
	if _, err := e.Run(ctx, "to-cancel", "v1", approval, input); !errors.Is(err, types.ErrSuspended) {
		t.Fatal(err)
	}
	if err := e.Cancel("to-cancel", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.Directory+"/stray-file", nil, 0o600); err != nil {
		t.Fatal(err)
	}

	all, err := e.List(nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, s := range all {
		got[s.RunID] = s.Status
	}
	want := map[string]string{"completed": statusCompleted, "suspended": statusSuspended, "to-cancel": statusCancelled}
	if len(got) != len(want) {
		t.Fatalf("List = %v, want %v", got, want)
	}
	for id, status := range want {
		if got[id] != status {
			t.Errorf("run %s status = %q, want %q", id, got[id], status)
		}
	}
	waiting, err := e.List(func(s State) bool { return s.Status == statusSuspended })
	if err != nil || len(waiting) != 1 || waiting[0].RunID != "suspended" {
		t.Errorf("filtered List = %+v, %v", waiting, err)
	}

	for _, tc := range []struct {
		id, revision string
		want         error
	}{
		{"suspended", "v1", ErrConflict},
		{"completed", "v2", ErrConflict},
		{"completed", "v1", nil},
		{"to-cancel", "v1", nil},
	} {
		if err := e.Delete(tc.id, tc.revision); !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
			t.Errorf("Delete(%s, %s) = %v, want %v", tc.id, tc.revision, err, tc.want)
		}
	}
	if _, err := e.Inspect("completed"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Inspect after Delete = %v, want os.ErrNotExist", err)
	}
	if _, err := e.Inspect("suspended"); err != nil {
		t.Errorf("suspended run lost: %v", err)
	}
	if err := e.Delete("missing", "v1"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Delete of a missing run = %v, want os.ErrNotExist", err)
	}
	if remaining, _ := e.List(nil); len(remaining) != 1 {
		t.Errorf("runs after deletes = %d, want 1", len(remaining))
	}
}
