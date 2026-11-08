package inproc_test

import (
	"context"
	"errors"
	"testing"

	"github.com/urmzd/saige/sandbox/node"
	"github.com/urmzd/saige/sandbox/node/inproc"
	"github.com/urmzd/saige/sandbox/sandbox"
	"github.com/urmzd/saige/sandbox/task"
	"github.com/urmzd/saige/sandbox/tool"
	"github.com/urmzd/saige/sandbox/workspace"
)

// CheckSpawn is the decision Spawn makes before it dispatches. For a node
// with spawning switched off, a target the policy does not grant, a node with
// no policy, and a granted target, the two must agree, and CheckSpawn must
// never dispatch.
func TestCheckSpawnDecidesExactlyAsSpawn(t *testing.T) {
	granted := sandbox.FromPolicies([]sandbox.Policy{{Tool: "parent", Spawn: []string{"child"}}})
	tests := []struct {
		name    string
		pdp     sandbox.PDP
		spawner bool
		target  string
		allowed bool
	}{
		{"spawning disabled", granted, false, "child", false},
		{"target not granted", granted, true, "stranger", false},
		{"no policy", nil, true, "child", false},
		{"target granted", granted, true, "child", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			ws, err := workspace.NewLocal(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			var dispatched []string
			spec := node.Spec{Deployment: "svc", PDP: tc.pdp}
			if tc.spawner {
				spec.Spawn = func(_ context.Context, tk task.Task) (string, error) {
					dispatched = append(dispatched, tk.Tool)
					return "task-1", nil
				}
			}
			var checkErr, spawnErr error
			reg := tool.NewRegistry()
			if err := reg.Register(tool.Func("parent", func(ctx context.Context, rt tool.Runtime, _ []byte) ([]byte, error) {
				checker, ok := rt.(tool.SpawnChecker)
				if !ok {
					t.Fatal("the in-process runtime does not implement tool.SpawnChecker")
				}
				checkErr = checker.CheckSpawn(tc.target)
				if len(dispatched) != 0 {
					t.Error("CheckSpawn dispatched a task")
				}
				_, spawnErr = rt.Spawn(ctx, task.Task{Tool: tc.target})
				return nil, nil
			})); err != nil {
				t.Fatal(err)
			}
			n, err := inproc.NewFactory(reg, ws).New(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := n.Run(ctx, task.Task{Tool: "parent"}); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if (checkErr == nil) != tc.allowed || (spawnErr == nil) != tc.allowed {
				t.Fatalf("CheckSpawn = %v, Spawn = %v, want allowed %v from both", checkErr, spawnErr, tc.allowed)
			}
			if !tc.allowed {
				if !errors.Is(checkErr, sandbox.ErrDenied) || checkErr.Error() != spawnErr.Error() {
					t.Errorf("CheckSpawn = %v, Spawn = %v, want the same ErrDenied", checkErr, spawnErr)
				}
			}
			if want := map[bool]int{true: 1, false: 0}[tc.allowed]; len(dispatched) != want {
				t.Errorf("dispatched %v, want %d task", dispatched, want)
			}
		})
	}
}
