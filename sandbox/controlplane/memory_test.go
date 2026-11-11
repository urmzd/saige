package controlplane_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/sandbox/controlplane"
	"github.com/urmzd/saige/sandbox/metrics"
	"github.com/urmzd/saige/sandbox/ngac"
	"github.com/urmzd/saige/sandbox/node/inproc"
	"github.com/urmzd/saige/sandbox/sandbox"
	"github.com/urmzd/saige/sandbox/task"
	"github.com/urmzd/saige/sandbox/tool"
	"github.com/urmzd/saige/sandbox/workspace"
)

// newPlane builds a control plane with an echo tool and a parent tool that
// spawns a sub-task for whatever tool name its input carries.
func newPlane(t *testing.T) (*controlplane.Memory, *metrics.Memory) {
	t.Helper()
	ws, err := workspace.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry()
	tools := []tool.Tool{
		tool.Func("echo", func(_ context.Context, _ tool.Runtime, in []byte) ([]byte, error) {
			return in, nil
		}),
		tool.Func("parent", func(ctx context.Context, rt tool.Runtime, in []byte) ([]byte, error) {
			id, err := rt.Spawn(ctx, task.Task{Tool: string(in), Input: []byte("from parent")})
			if err != nil {
				return nil, err
			}
			return []byte(id), nil
		}),
		tool.Func("save", func(ctx context.Context, rt tool.Runtime, in []byte) ([]byte, error) {
			if err := rt.Workspace().Write(ctx, "shared/out", strings.NewReader(string(in))); err != nil {
				return nil, err
			}
			return []byte("saved"), nil
		}),
	}
	for _, tl := range tools {
		if err := registry.Register(tl); err != nil {
			t.Fatal(err)
		}
	}
	rec := metrics.NewMemory()
	return controlplane.NewMemory(inproc.NewFactory(registry, ws), rec), rec
}

func TestDeployScaleStatus(t *testing.T) {
	ctx := context.Background()
	plane, rec := newPlane(t)

	spec := controlplane.ServiceSpec{
		Name:     "svc",
		Replicas: 2,
		Policies: []sandbox.Policy{{Tool: "echo", Areas: []sandbox.Area{{Prefix: "echo"}}}},
	}
	if err := plane.Deploy(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := plane.Deploy(ctx, spec); !errors.Is(err, controlplane.ErrExists) {
		t.Fatalf("duplicate deploy: want ErrExists, got %v", err)
	}

	if err := plane.Scale(ctx, "svc", 5); err != nil {
		t.Fatal(err)
	}
	status, err := plane.Status(ctx, "svc")
	if err != nil {
		t.Fatal(err)
	}
	if status.Replicas != 5 || len(status.Nodes) != 5 {
		t.Fatalf("after scale up: %+v", status)
	}

	if err := plane.Scale(ctx, "svc", 1); err != nil {
		t.Fatal(err)
	}
	status, err = plane.Status(ctx, "svc")
	if err != nil {
		t.Fatal(err)
	}
	if status.Replicas != 1 {
		t.Fatalf("after scale down: %+v", status)
	}

	if got := rec.Snapshot()[`sandbox_nodes{deployment="svc"}`]; got != 1 {
		t.Fatalf("sandbox_nodes gauge = %v, want 1", got)
	}

	if err := plane.Scale(ctx, "missing", 1); !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("scale unknown: want ErrNotFound, got %v", err)
	}
}

func TestSubmitRunsOnTheDeploymentsNodes(t *testing.T) {
	ctx := context.Background()
	plane, rec := newPlane(t)

	if err := plane.Deploy(ctx, controlplane.ServiceSpec{Name: "svc", Replicas: 3}); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 6; i++ {
		res, err := plane.Submit(ctx, "svc", task.Task{Tool: "echo", Input: []byte("hi")})
		if err != nil {
			t.Fatal(err)
		}
		if string(res.Output) != "hi" || res.NodeID == "" {
			t.Fatalf("result = %+v", res)
		}
	}

	key := `sandbox_tasks_total{deployment="svc",status="ok",tool="echo"}`
	if got := rec.Snapshot()[key]; got != 6 {
		t.Fatalf("%s = %v, want 6", key, got)
	}
}

func TestSubmitAsyncAndResult(t *testing.T) {
	ctx := context.Background()
	plane, _ := newPlane(t)

	if err := plane.Deploy(ctx, controlplane.ServiceSpec{Name: "svc"}); err != nil {
		t.Fatal(err)
	}
	id, err := plane.SubmitAsync(ctx, "svc", task.Task{Tool: "echo", Input: []byte("later")})
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("empty task id")
	}

	res := awaitResult(t, plane, "svc", id)
	if string(res.Output) != "later" {
		t.Fatalf("result = %+v", res)
	}
}

func TestSpawnIsPolicyGated(t *testing.T) {
	ctx := context.Background()

	t.Run("allowed", func(t *testing.T) {
		plane, _ := newPlane(t)
		err := plane.Deploy(ctx, controlplane.ServiceSpec{
			Name: "svc",
			Policies: []sandbox.Policy{
				{Tool: "parent", Spawn: []string{"echo"}},
				{Tool: "echo"},
			},
		})
		if err != nil {
			t.Fatal(err)
		}

		// The parent tool spawns an echo sub-task and returns its ID.
		res, err := plane.Submit(ctx, "svc", task.Task{Tool: "parent", Input: []byte("echo")})
		if err != nil {
			t.Fatal(err)
		}
		child := awaitResult(t, plane, "svc", string(res.Output))
		if string(child.Output) != "from parent" {
			t.Fatalf("child result = %+v", child)
		}
	})

	t.Run("denied without allowlist", func(t *testing.T) {
		plane, _ := newPlane(t)
		err := plane.Deploy(ctx, controlplane.ServiceSpec{
			Name:     "svc",
			Policies: []sandbox.Policy{{Tool: "parent"}}, // no Spawn list
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = plane.Submit(ctx, "svc", task.Task{Tool: "parent", Input: []byte("echo")})
		if err == nil || !strings.Contains(err.Error(), "may not spawn") {
			t.Fatalf("want spawn denial, got %v", err)
		}
	})
}

func TestDeployWithNGACAccess(t *testing.T) {
	ctx := context.Background()

	access := &ngac.Spec{
		PolicyClasses: []string{"pc"},
		UserAttrs:     []ngac.Node{{Name: "agents"}},
		Users: []ngac.Node{
			{Name: "save", Parents: []string{"agents"}},
			{Name: "echo", Parents: []string{"agents"}},
		},
		ObjectAttrs: []ngac.ObjectAttr{
			{Name: "shared", Prefix: "shared", Parents: []string{"pc"}},
		},
		Associations: []ngac.Association{
			{UserAttr: "agents", Ops: []string{sandbox.OpRead, sandbox.OpWrite}, Target: "shared"},
		},
	}

	t.Run("granted through user attribute", func(t *testing.T) {
		plane, _ := newPlane(t)
		if err := plane.Deploy(ctx, controlplane.ServiceSpec{Name: "svc", Access: access}); err != nil {
			t.Fatal(err)
		}
		res, err := plane.Submit(ctx, "svc", task.Task{Tool: "save", Input: []byte("v1")})
		if err != nil {
			t.Fatal(err)
		}
		if string(res.Output) != "saved" {
			t.Fatalf("result = %+v", res)
		}
	})

	t.Run("prohibition overrides", func(t *testing.T) {
		denied := *access
		denied.Prohibitions = []ngac.Association{
			{UserAttr: "save", Ops: []string{sandbox.OpWrite}, Target: "shared"},
		}
		plane, _ := newPlane(t)
		if err := plane.Deploy(ctx, controlplane.ServiceSpec{Name: "svc", Access: &denied}); err != nil {
			t.Fatal(err)
		}
		_, err := plane.Submit(ctx, "svc", task.Task{Tool: "save", Input: []byte("v1")})
		if err == nil || !strings.Contains(err.Error(), "denied") {
			t.Fatalf("want denial, got %v", err)
		}
	})

	t.Run("policies and access are mutually exclusive", func(t *testing.T) {
		plane, _ := newPlane(t)
		err := plane.Deploy(ctx, controlplane.ServiceSpec{
			Name:     "svc",
			Access:   access,
			Policies: []sandbox.Policy{{Tool: "echo"}},
		})
		if err == nil || !strings.Contains(err.Error(), "not both") {
			t.Fatalf("want mutual-exclusion error, got %v", err)
		}
	})
}

func TestSubmitUnknownDeployment(t *testing.T) {
	plane, _ := newPlane(t)
	_, err := plane.Submit(context.Background(), "nope", task.Task{Tool: "echo"})
	if !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// awaitResult polls for an async task's result.
func awaitResult(t *testing.T, plane *controlplane.Memory, name, id string) task.Result {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		res, done, err := plane.Result(context.Background(), name, id)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			return res
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("task %s never completed", id)
	return task.Result{}
}
