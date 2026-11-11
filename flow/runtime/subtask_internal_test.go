package runtime

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/urmzd/saige/sandbox/controlplane"
	"github.com/urmzd/saige/sandbox/metrics"
	"github.com/urmzd/saige/sandbox/node/inproc"
	dispatchsandbox "github.com/urmzd/saige/sandbox/sandbox"
	"github.com/urmzd/saige/sandbox/task"
	"github.com/urmzd/saige/sandbox/tool"
	"github.com/urmzd/saige/sandbox/workspace"

	"github.com/urmzd/saige/flow/turn/native"
)

// parentRuntime is the runtime a parent turn's node hands it: its scoped
// workspace view, and a Spawn that records what reached it.
type parentRuntime struct {
	ws      workspace.Workspace
	mu      sync.Mutex
	spawned []string
}

func (p *parentRuntime) Workspace() workspace.Workspace { return p.ws }

func (p *parentRuntime) Spawn(_ context.Context, t task.Task) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.spawned = append(p.spawned, t.Tool)
	return "task-1", nil
}

// subTaskWorld deploys one service whose policy grants the parent "ops" the
// areas shared/ and parent/ and the spawn of "helper", and grants "helper"
// shared/ and child/ and the spawn of "third". It returns the sandbox, the
// parent's runtime, and the raw workspace beneath both.
func subTaskWorld(t *testing.T) (*Sandbox, *parentRuntime, Request, workspace.Workspace) {
	t.Helper()
	ctx := context.Background()
	raw, err := workspace.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tools := tool.NewRegistry()
	for _, name := range []string{"ops", "helper", "third", "intruder"} {
		if err := tools.Register(tool.Func(ToolName(name), func(context.Context, tool.Runtime, []byte) ([]byte, error) { return nil, nil })); err != nil {
			t.Fatal(err)
		}
	}
	plane := controlplane.NewMemory(inproc.NewFactory(tools, raw), metrics.Nop())
	if err := plane.Deploy(ctx, controlplane.ServiceSpec{
		Name:     "svc",
		Replicas: 1,
		Policies: []dispatchsandbox.Policy{
			{Tool: ToolName("ops"), Areas: []dispatchsandbox.Area{{Prefix: "shared"}, {Prefix: "parent"}}, Spawn: []string{ToolName("helper")}},
			{Tool: ToolName("helper"), Areas: []dispatchsandbox.Area{{Prefix: "shared"}, {Prefix: "child"}}, Spawn: []string{ToolName("third")}},
		},
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	spec, err := plane.Spec(ctx, "svc")
	if err != nil {
		t.Fatal(err)
	}
	pdp, err := spec.PDP()
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSandbox(plane, tools, nil)
	if err != nil {
		t.Fatal(err)
	}
	parent := &parentRuntime{ws: dispatchsandbox.ScopePDP(raw, ToolName("ops"), pdp)}
	req := Request{Service: "svc", Definition: native.Revision{Name: "ops", Version: "1.0.0", Spec: native.Spec{Name: "ops"}}}
	return s, parent, req, raw
}

func write(ws workspace.Workspace, key string) error {
	return ws.Write(context.Background(), key, strings.NewReader("x"))
}

func read(ws workspace.Workspace, key string) error {
	r, err := ws.Read(context.Background(), key)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, r)
	return r.Close()
}

// A sub-task running on its parent's node touches only what both it and its
// parent are granted: its own grant never widens the parent's view, and the
// parent's grant never widens its own. The parent keeps exactly its own.
func TestASubTaskOnItsParentsNodeIsConfinedToWhatBothMayTouch(t *testing.T) {
	s, parent, req, raw := subTaskWorld(t)
	for _, key := range []string{"shared/seed", "parent/seed", "child/seed", "other/seed"} {
		if err := write(raw, key); err != nil {
			t.Fatal(err)
		}
	}
	child, err := s.subTaskRuntime(context.Background(), parent, req, "helper")
	if err != nil {
		t.Fatalf("subTaskRuntime: %v", err)
	}

	tests := []struct {
		who     string
		ws      workspace.Workspace
		key     string
		allowed bool
	}{
		{"child", child.Workspace(), "shared/out", true},
		{"child", child.Workspace(), "child/out", false},  // the parent's view has no child/
		{"child", child.Workspace(), "parent/out", false}, // the child has no grant to parent/
		{"child", child.Workspace(), "other/out", false},
		{"parent", parent.Workspace(), "shared/out", true},
		{"parent", parent.Workspace(), "parent/out", true},
		{"parent", parent.Workspace(), "child/out", false},
		{"parent", parent.Workspace(), "other/out", false},
	}
	for _, tc := range tests {
		for op, do := range map[string]func(workspace.Workspace, string) error{"write": write, "read": read} {
			key := tc.key
			if op == "read" {
				key = strings.TrimSuffix(key, "out") + "seed"
			}
			err := do(tc.ws, key)
			switch {
			case tc.allowed && err != nil:
				t.Errorf("%s %s %s: %v, want allowed", tc.who, op, key, err)
			case !tc.allowed && !errors.Is(err, dispatchsandbox.ErrDenied):
				t.Errorf("%s %s %s: %v, want denied", tc.who, op, key, err)
			}
		}
	}

	keys, err := child.Workspace().List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if !strings.HasPrefix(k, "shared/") {
			t.Errorf("the sub-task can see %q outside what both may read", k)
		}
	}
}

// Spawning is gated twice: the parent must be granted the sub-task, and the
// sub-task's own spawns are held to its own grant before they reach the
// parent's node.
func TestASubTaskOnItsParentsNodeSpawnsOnlyWhatItIsGranted(t *testing.T) {
	s, parent, req, _ := subTaskWorld(t)
	if _, err := s.subTaskRuntime(context.Background(), parent, req, "intruder"); !errors.Is(err, dispatchsandbox.ErrDenied) {
		t.Fatalf("a sub-task the parent may not spawn = %v, want denied", err)
	}
	child, err := s.subTaskRuntime(context.Background(), parent, req, "helper")
	if err != nil {
		t.Fatalf("subTaskRuntime: %v", err)
	}
	if _, err := child.Spawn(context.Background(), task.Task{Tool: ToolName("ops")}); !errors.Is(err, dispatchsandbox.ErrDenied) {
		t.Errorf("the sub-task spawning its parent = %v, want denied", err)
	}
	if _, err := child.Spawn(context.Background(), task.Task{Tool: ToolName("third")}); err != nil {
		t.Errorf("the sub-task spawning what it is granted = %v, want allowed", err)
	}
	if got := parent.spawned; len(got) != 1 || got[0] != ToolName("third") {
		t.Errorf("the parent's node was asked to spawn %v, want only the granted one", got)
	}
}
