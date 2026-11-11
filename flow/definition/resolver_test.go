package definition_test

import (
	"context"
	"errors"
	"testing"

	"github.com/urmzd/saige/durable/run"

	"github.com/urmzd/saige/flow/definition"
	"github.com/urmzd/saige/flow/definition/definitiontest"
)

// tampered is a Source whose Get returns other content for one version: the
// corruption an insert-only store is built to rule out.
type tampered struct {
	definition.Source
	name, version string
	edit          func(definition.Record) definition.Record
}

func (s tampered) Get(ctx context.Context, tenant, name, version string) (definition.Record, error) {
	r, err := s.Source.Get(ctx, tenant, name, version)
	if err == nil && name == s.name && version == s.version {
		r = s.edit(r)
	}
	return r, err
}

func put(t *testing.T, src definition.Source, rec definition.Record) definition.Record {
	t.Helper()
	got, err := src.Put(context.Background(), rec)
	if err != nil {
		t.Fatalf("put %s: %v", rec.Content, err)
	}
	return got
}

func withSubagent(name, version, sub string) string {
	return "---\napiVersion: saige/v1\nname: " + name + "\nversion: " + version + "\nmodel: offline/echo\nsubagents:\n  - ref: " + sub + "\n---\nDelegate.\n"
}

func TestResolvePinsTheWholeTree(t *testing.T) {
	ctx := context.Background()
	src := definition.NewMemory()
	put(t, src, definitiontest.Record("acme", "writer", "1.0.0", "Write."))
	put(t, src, definitiontest.Record("acme", "writer", "1.1.0", "Write well."))
	put(t, src, definition.Record{Tenant: "acme", Content: withSubagent("router", "1.0.0", "writer@~1.0"), CreatedBy: "ada"})
	r := definition.NewResolver(src, definition.Checks{})
	pin, err := r.Resolve(ctx, "acme", "router")
	if err != nil {
		t.Fatal(err)
	}
	if pin.Name != "router" || pin.Version != "1.0.0" || pin.Digest == "" || pin.Hash == "" || pin.PolicyHash == "" {
		t.Fatalf("pin = %+v", pin)
	}
	if v, ok := pin.SubagentVersion("writer"); !ok || v != "1.0.0" {
		t.Errorf("writer pinned at %q, want the range's 1.0.0", v)
	}
	loaded, err := r.Verify(ctx, "acme", pin)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if loaded.Agent.Spec.Name != "router" || loaded.Agent.Spec.Access.Spawn[0] != "writer" {
		t.Errorf("loaded %+v", loaded.Agent.Spec)
	}
	if _, err := r.Resolve(ctx, "globex", "router"); !errors.Is(err, definition.ErrNotFound) {
		t.Errorf("resolve in another tenant = %v", err)
	}
}

func TestVerifyFailsClosedOnEmptyDigest(t *testing.T) {
	ctx := context.Background()
	src := definition.NewMemory()
	put(t, src, definitiontest.Record("acme", "docs-bot", "1.0.0", "Answer."))
	r := definition.NewResolver(src, definition.Checks{})
	pin, err := r.Resolve(ctx, "acme", "docs-bot")
	if err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*definition.Pin){
		"empty digest":      func(p *definition.Pin) { p.Digest = "" },
		"empty policy hash": func(p *definition.Pin) { p.PolicyHash = "" },
		"empty hash":        func(p *definition.Pin) { p.Hash = "" },
		"no version":        func(p *definition.Pin) { p.Version = "" },
		"forged digest":     func(p *definition.Pin) { p.Digest = "sha256:00" },
	} {
		bad := pin
		edit(&bad)
		_, err := r.Verify(ctx, "acme", bad)
		if !errors.Is(err, definition.ErrPin) || !run.IsNonRetryable(err) {
			t.Errorf("%s: verify = %v, want a terminal ErrPin", name, err)
		}
	}
	missing := pin
	missing.Version = "9.0.0"
	missing.Hash = ""
	if _, err := r.Verify(ctx, "acme", missing); !run.IsNonRetryable(err) {
		t.Errorf("verify of an unhashed pin = %v, want terminal", err)
	}
	if _, err := r.Verify(ctx, "globex", pin); !run.IsNonRetryable(err) {
		t.Errorf("verify in another tenant = %v, want a terminal missing row", err)
	}
}

func TestVerifyDetectsSubagentChange(t *testing.T) {
	ctx := context.Background()
	src := definition.NewMemory()
	put(t, src, definitiontest.Record("acme", "writer", "1.0.0", "Write."))
	put(t, src, definition.Record{Tenant: "acme", Content: withSubagent("router", "1.0.0", "writer"), CreatedBy: "ada"})
	pin, err := definition.NewResolver(src, definition.Checks{}).Resolve(ctx, "acme", "router")
	if err != nil {
		t.Fatal(err)
	}

	// A newer writer does not move the pin.
	put(t, src, definitiontest.Record("acme", "writer", "1.1.0", "Write better."))
	if _, err := definition.NewResolver(src, definition.Checks{}).Verify(ctx, "acme", pin); err != nil {
		t.Errorf("a newer sub-agent version broke the pin: %v", err)
	}

	// The pinned writer's content changing under its version does.
	bad := tampered{Source: src, name: "writer", version: "1.0.0", edit: func(r definition.Record) definition.Record {
		r.Content = definitiontest.Content("writer", "1.0.0", "Write something else.")
		return r
	}}
	_, err = definition.NewResolver(bad, definition.Checks{}).Verify(ctx, "acme", pin)
	if !errors.Is(err, definition.ErrPin) || !run.IsNonRetryable(err) {
		t.Errorf("verify after the sub-agent changed = %v, want a terminal ErrPin", err)
	}

	// So does a pin that names another sub-agent version.
	moved := pin
	moved.Subagents = []definition.Version{{Name: "writer", Version: "1.1.0"}}
	if _, err := definition.NewResolver(src, definition.Checks{}).Verify(ctx, "acme", moved); !errors.Is(err, definition.ErrPin) {
		t.Errorf("verify of a pin edited to a newer sub-agent = %v", err)
	}
}

func TestVerifyDetectsAPolicyEdit(t *testing.T) {
	ctx := context.Background()
	src := definition.NewMemory()
	put(t, src, definitiontest.Record("acme", "docs-bot", "1.0.0", "Answer."))
	pin, err := definition.NewResolver(src, definition.Checks{}).Resolve(ctx, "acme", "docs-bot")
	if err != nil {
		t.Fatal(err)
	}
	bad := tampered{Source: src, name: "docs-bot", version: "1.0.0", edit: func(r definition.Record) definition.Record {
		r.Policy.WorkspaceAreas = []string{"everything/"}
		return r
	}}
	_, err = definition.NewResolver(bad, definition.Checks{}).Verify(ctx, "acme", pin)
	if !errors.Is(err, definition.ErrPin) || !run.IsNonRetryable(err) {
		t.Errorf("verify after a policy edit = %v, want a terminal ErrPin", err)
	}
}

func TestResolveRefusesRetired(t *testing.T) {
	ctx := context.Background()
	src := definition.NewMemory()
	put(t, src, definitiontest.Record("acme", "writer", "1.0.0", "Write."))
	put(t, src, definition.Record{Tenant: "acme", Content: withSubagent("router", "1.0.0", "writer"), CreatedBy: "ada"})
	r := definition.NewResolver(src, definition.Checks{})
	pin, err := r.Resolve(ctx, "acme", "router")
	if err != nil {
		t.Fatal(err)
	}
	if err := src.Retire(ctx, "acme", "writer", "ada"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(ctx, "acme", "router"); !errors.Is(err, definition.ErrRetired) {
		t.Errorf("resolve through a retired sub-agent = %v, want ErrRetired", err)
	}
	if _, err := r.Verify(ctx, "acme", pin); err != nil {
		t.Errorf("a pinned run lost its retired sub-agent: %v", err)
	}
}
