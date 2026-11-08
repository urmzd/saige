package isolate_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/urmzd/saige/sandbox/isolate"
	"github.com/urmzd/saige/sandbox/isolate/local"
	"github.com/urmzd/saige/sandbox/runner"
)

func TestExecutorRunsShellInALease(t *testing.T) {
	pool := isolate.NewPool(local.Driver{Log: io.Discard}, isolate.PoolConfig{})
	ctx := context.Background()
	defer pool.Close(ctx) //nolint:errcheck // test cleanup
	lease, err := pool.Lease(ctx, isolate.Spec{Image: "host"})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release(ctx) //nolint:errcheck // test cleanup

	sh := runner.Shell("sh", isolate.Executor{Instance: lease}, runner.ShellConfig{
		Env: map[string]string{"PATH": os.Getenv("PATH")},
	})
	raw, err := sh.Call(ctx, nil, []byte(`{"script":"echo out; echo err >&2; echo kept > f; exit 2"}`))
	if err != nil {
		t.Fatal(err)
	}
	var out runner.ShellOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.ExitCode != 2 || out.Logs != "out\nerr\n" && out.Logs != "err\nout\n" {
		t.Fatalf("out = %+v", out)
	}
	// Runs share the lease's work directory.
	rc, err := lease.Files().Read(ctx, "f")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "kept\n" {
		t.Fatalf("f = %q", got)
	}
}

func TestExecutorMapsErrors(t *testing.T) {
	inst, err := local.Driver{Log: io.Discard}.Start(context.Background(), isolate.Spec{Image: "host"})
	if err != nil {
		t.Fatal(err)
	}
	ex := isolate.Executor{Instance: inst}
	ctx := context.Background()
	if _, err := ex.Run(ctx, runner.Request{}, io.Discard); !errors.Is(err, runner.ErrInvalidRequest) {
		t.Errorf("empty: %v", err)
	}
	if _, err := ex.Run(ctx, runner.Request{Command: []string{"/no/such/binary"}}, io.Discard); !errors.Is(err, runner.ErrStart) {
		t.Errorf("missing binary: %v", err)
	}
	_, err = ex.Run(ctx, runner.Request{Command: []string{"/bin/sh", "-c", "sleep 5"}, Timeout: 100 * time.Millisecond}, io.Discard)
	if !errors.Is(err, runner.ErrTimeout) || !errors.Is(err, isolate.ErrTimeout) {
		t.Errorf("timeout: %v", err)
	}
	if err := inst.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.Run(ctx, runner.Request{Command: []string{"true"}}, io.Discard); !errors.Is(err, runner.ErrStart) || !errors.Is(err, isolate.ErrClosed) {
		t.Errorf("closed: %v", err)
	}
}
