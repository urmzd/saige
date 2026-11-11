package sandbox_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/urmzd/saige/sandbox/sandbox"
	"github.com/urmzd/saige/sandbox/workspace"
)

func newWorkspace(t *testing.T) workspace.Workspace {
	t.Helper()
	ws, err := workspace.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func write(t *testing.T, ws workspace.Workspace, key, val string) {
	t.Helper()
	if err := ws.Write(context.Background(), key, strings.NewReader(val)); err != nil {
		t.Fatalf("write %q: %v", key, err)
	}
}

func TestScopeConfinesAccess(t *testing.T) {
	ctx := context.Background()
	raw := newWorkspace(t)
	write(t, raw, "shared/config", "cfg")
	write(t, raw, "secrets/token", "hunter2")

	scoped := sandbox.Scope(raw, sandbox.Policy{
		Tool:  "worker",
		Areas: []sandbox.Area{{Prefix: "shared"}},
	})

	tests := []struct {
		name string
		op   func() error
		deny bool
	}{
		{"read inside area", func() error { rc, err := scoped.Read(ctx, "shared/config"); closeIf(rc, err); return err }, false},
		{"write inside area", func() error { return scoped.Write(ctx, "shared/out", strings.NewReader("x")) }, false},
		{"read outside area", func() error { rc, err := scoped.Read(ctx, "secrets/token"); closeIf(rc, err); return err }, true},
		{"write outside area", func() error { return scoped.Write(ctx, "secrets/new", strings.NewReader("x")) }, true},
		{"delete outside area", func() error { return scoped.Delete(ctx, "secrets/token") }, true},
		{"sibling prefix does not match", func() error { rc, err := scoped.Read(ctx, "shared-extra/x"); closeIf(rc, err); return err }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.op()
			if tt.deny && !errors.Is(err, sandbox.ErrDenied) {
				t.Fatalf("want ErrDenied, got %v", err)
			}
			if !tt.deny && errors.Is(err, sandbox.ErrDenied) {
				t.Fatalf("unexpected denial: %v", err)
			}
		})
	}
}

func TestScopeRejectsTraversal(t *testing.T) {
	ctx := context.Background()
	scoped := sandbox.Scope(newWorkspace(t), sandbox.Policy{
		Tool:  "worker",
		Areas: []sandbox.Area{{Prefix: "shared"}},
	})

	for _, key := range []string{"shared/../secrets/token", "/etc/passwd", "shared/./x", ""} {
		if _, err := scoped.Read(ctx, key); !errors.Is(err, workspace.ErrInvalidKey) {
			t.Errorf("Read(%q): want ErrInvalidKey, got %v", key, err)
		}
	}
}

func TestScopeReadOnlyArea(t *testing.T) {
	ctx := context.Background()
	raw := newWorkspace(t)
	write(t, raw, "docs/readme", "hi")

	scoped := sandbox.Scope(raw, sandbox.Policy{
		Tool:  "reader",
		Areas: []sandbox.Area{{Prefix: "docs", ReadOnly: true}},
	})

	if rc, err := scoped.Read(ctx, "docs/readme"); err != nil {
		t.Fatalf("read in read-only area: %v", err)
	} else {
		_ = rc.Close()
	}
	if err := scoped.Write(ctx, "docs/new", strings.NewReader("x")); !errors.Is(err, sandbox.ErrDenied) {
		t.Fatalf("write to read-only area: want ErrDenied, got %v", err)
	}
	if err := scoped.Delete(ctx, "docs/readme"); !errors.Is(err, sandbox.ErrDenied) {
		t.Fatalf("delete in read-only area: want ErrDenied, got %v", err)
	}
}

func TestScopeListHidesOtherAreas(t *testing.T) {
	ctx := context.Background()
	raw := newWorkspace(t)
	write(t, raw, "shared/a", "1")
	write(t, raw, "secrets/token", "2")

	scoped := sandbox.Scope(raw, sandbox.Policy{
		Tool:  "worker",
		Areas: []sandbox.Area{{Prefix: "shared"}},
	})
	keys, err := scoped.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != "shared/a" {
		t.Fatalf("scoped list leaked keys: %v", keys)
	}
}

func TestEmptyPolicyDeniesAll(t *testing.T) {
	ctx := context.Background()
	raw := newWorkspace(t)
	write(t, raw, "shared/a", "1")

	scoped := sandbox.Scope(raw, sandbox.Policy{Tool: "unbound"})
	if _, err := scoped.Read(ctx, "shared/a"); !errors.Is(err, sandbox.ErrDenied) {
		t.Fatalf("want ErrDenied, got %v", err)
	}
	keys, err := scoped.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Fatalf("empty policy leaked keys: %v", keys)
	}
}

func closeIf(rc interface{ Close() error }, err error) {
	if err == nil && rc != nil {
		_ = rc.Close()
	}
}
