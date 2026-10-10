package definition

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func def(t *testing.T, name, version, extra string) *Definition {
	t.Helper()
	d, err := Parse([]byte("---\napiVersion: saige/v1\nname: "+name+"\nversion: "+version+"\n"+extra+"---\nI am "+name+" "+version+".\n"), name+"@"+version)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func loaded(t *testing.T, checks Checks, defs ...*Definition) *Registry {
	t.Helper()
	r := NewRegistry(StaticSource(defs...), checks)
	if _, err := r.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestResolveVersions(t *testing.T) {
	r := loaded(t, Checks{},
		def(t, "a", "1.0.0", ""), def(t, "a", "1.4.2", ""), def(t, "a", "2.0.0", ""),
		def(t, "a", "2.1.0-rc.1", ""), def(t, "b", "0.1.0", ""))
	cases := map[string]string{
		"a": "2.0.0", "a@^1": "1.4.2", "a@~1.0": "1.0.0", "a@1.4.2": "1.4.2", "a@>=1.1.0 <2.0.0": "1.4.2",
		"a@2.1.0-rc.1": "2.1.0-rc.1", "a@^2.1.0-rc.1": "2.1.0-rc.1", "b@*": "0.1.0",
	}
	for ref, want := range cases {
		res, err := r.Resolve(ref)
		if err != nil || res.Version != want {
			t.Errorf("%s: got %v %v, want %s", ref, res, err, want)
		}
	}
	_, err := r.Resolve("a@^3")
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "available: 2.1.0-rc.1, 2.0.0, 1.4.2, 1.0.0") {
		t.Fatalf("got %v", err)
	}
	_, err = r.Resolve("aa")
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), `did you mean "a"`) {
		t.Fatalf("got %v", err)
	}
	if _, err := r.Resolve("A"); err == nil {
		t.Fatal("an invalid reference resolved")
	}
	if got := len(r.List()); got != 5 {
		t.Fatalf("List returned %d", got)
	}
}

func TestResolveNotLoaded(t *testing.T) {
	if _, err := NewRegistry(StaticSource(), Checks{}).Resolve("a"); err == nil {
		t.Fatal("an unloaded registry resolved")
	}
}

func TestResolveSubagentsAndDigest(t *testing.T) {
	leaf := def(t, "leaf", "1.0.0", "")
	mid := def(t, "mid", "1.0.0", "subagents: [leaf@^1]\n")
	top := def(t, "top", "1.0.0", "subagents:\n  - ref: mid\n    mode: spawn\n  - leaf\n")
	r := loaded(t, Checks{}, leaf, mid, top)
	res, err := r.Resolve("top")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Subagents) != 2 || res.Subagents[0].Agent.Name != "mid" || res.Subagents[0].Mode != "spawn" ||
		res.Subagents[0].Agent.Subagents[0].Agent != res.Subagents[1].Agent {
		t.Fatalf("got %+v", res.Subagents)
	}
	if res.Digest == res.Definition.Digest || !strings.HasPrefix(res.Digest, "sha256:") {
		t.Fatalf("digest %s", res.Digest)
	}
	again, _ := r.Resolve("top@1.0.0")
	if again != res {
		t.Fatal("a second resolution was not the same pinned value")
	}

	// A new leaf version changes every digest above it.
	leaf2 := def(t, "leaf", "1.1.0", "")
	r2 := loaded(t, Checks{}, leaf, leaf2, mid, top)
	res2, _ := r2.Resolve("top")
	if res2.Definition.Digest != res.Definition.Digest || res2.Digest == res.Digest {
		t.Fatalf("closure digest did not follow the sub-agent: %s %s", res.Digest, res2.Digest)
	}
}

func TestLoadRejectsBrokenReferences(t *testing.T) {
	r := NewRegistry(StaticSource(def(t, "a", "1.0.0", "subagents: [missing@^1, b@^2]\n"), def(t, "b", "1.0.0", "")), Checks{})
	_, err := r.Load(context.Background())
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Issues) != 2 || ve.Issues[0].Code != CodeUnresolved ||
		!strings.Contains(err.Error(), `no definition named "missing"`) || !strings.Contains(err.Error(), "available: 1.0.0") {
		t.Fatalf("got %v", err)
	}
}

func TestLoadRejectsCycles(t *testing.T) {
	r := NewRegistry(StaticSource(
		def(t, "a", "1.0.0", "subagents: [b]\n"),
		def(t, "b", "1.0.0", "subagents: [c]\n"),
		def(t, "c", "1.0.0", "subagents: [a@^1]\n"),
	), Checks{})
	_, err := r.Load(context.Background())
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Issues[0].Code != CodeCycle || !strings.Contains(err.Error(), "a@1.0.0 -> b@1.0.0 -> c@1.0.0 -> a@1.0.0") {
		t.Fatalf("got %v", err)
	}
	// A cycle only through a version no range picks is not a cycle.
	r = NewRegistry(StaticSource(
		def(t, "a", "1.0.0", "subagents: [b]\n"),
		def(t, "b", "1.0.0", "subagents: [a@^2]\n"),
		def(t, "a", "2.0.0", ""),
	), Checks{})
	if _, err := r.Load(context.Background()); err != nil {
		t.Fatalf("got %v", err)
	}
}

func TestLoadRejectsDuplicates(t *testing.T) {
	r := NewRegistry(StaticSource(def(t, "a", "1.0.0", ""), def(t, "a", "1.0.0", "description: other\n")), Checks{})
	if _, err := r.Load(context.Background()); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("got %v", err)
	}
}

func TestChecks(t *testing.T) {
	checks := Checks{
		Model: func(ref string) error {
			if ref == "nope" || ref == "x/missing" {
				return fmt.Errorf("unknown model %s", ref)
			}
			return nil
		},
		Skill: func(name, hash string) error {
			if name != "known" {
				return fmt.Errorf("unknown skill %s", name)
			}
			return nil
		},
	}
	r := NewRegistry(StaticSource(def(t, "a", "1.0.0", "model:\n  use: nope\n  fallback: [x/missing]\nskills: [known, unknown]\n")), checks)
	_, err := r.Load(context.Background())
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Issues) != 3 {
		t.Fatalf("got %v", err)
	}
	for i, path := range []string{"model.use", "model.fallback[0]", "skills[1]"} {
		if ve.Issues[i].Path != path || ve.Issues[i].Code != CodeUnresolved {
			t.Fatalf("issue %d: %+v", i, ve.Issues[i])
		}
	}
}

// mutableSource serves whatever the test sets.
type mutableSource struct {
	mu   sync.Mutex
	defs []*Definition
	err  error
	fire func()
}

func (m *mutableSource) set(err error, defs ...*Definition) {
	m.mu.Lock()
	m.defs, m.err = defs, err
	fire := m.fire
	m.mu.Unlock()
	if fire != nil {
		fire()
	}
}

func (m *mutableSource) Load(context.Context) ([]*Definition, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.defs, m.err
}

func (m *mutableSource) Watch(_ context.Context, fn func()) (func(), error) {
	m.mu.Lock()
	m.fire = fn
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		m.fire = nil
		m.mu.Unlock()
	}, nil
}

func TestPinningSurvivesReload(t *testing.T) {
	src := &mutableSource{}
	v1 := def(t, "a", "1.0.0", "")
	src.set(nil, v1)
	r := NewRegistry(src, Checks{})
	if _, err := r.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	pinned, _ := r.Resolve("a")

	// The same version edited in place: a reload changes what new
	// resolutions see, never the pinned one.
	edited, _ := Parse([]byte(minimal("a", "1.0.0")+"edited"), "a")
	src.set(nil, edited)
	changed, err := r.Load(context.Background())
	if err != nil || !changed {
		t.Fatalf("reload: %v %v", changed, err)
	}
	fresh, _ := r.Resolve("a")
	if fresh.Digest == pinned.Digest || !strings.Contains(fresh.Prompt, "edited") || strings.Contains(pinned.Prompt, "edited") {
		t.Fatalf("pinned %s %q, fresh %s %q", pinned.Digest, pinned.Prompt, fresh.Digest, fresh.Prompt)
	}
	if got, ok := r.Pinned(pinned.Digest); !ok || got != pinned {
		t.Fatal("the pinned resolution is not found by digest after a reload")
	}

	// A reload that fails keeps the previous contents.
	src.set(errors.New("source down"))
	if _, err := r.Load(context.Background()); err == nil {
		t.Fatal("a failed reload reported success")
	}
	if res, err := r.Resolve("a"); err != nil || res != fresh {
		t.Fatalf("after a failed reload: %v %v", res, err)
	}
	// So does one that breaks a reference.
	src.set(nil, def(t, "a", "1.0.0", "subagents: [ghost]\n"))
	if _, err := r.Load(context.Background()); err == nil {
		t.Fatal("a broken reload replaced the registry")
	}
	rev := r.Revision()
	src.set(nil, edited)
	if changed, _ := r.Load(context.Background()); changed || r.Revision() != rev {
		t.Fatal("an unchanged reload counted as a change")
	}
}

func TestWatchReloads(t *testing.T) {
	src := &mutableSource{}
	src.set(nil, def(t, "a", "1.0.0", ""))
	r := NewRegistry(src, Checks{})
	if _, err := r.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	reloads := make(chan bool, 4)
	stop, err := r.Watch(context.Background(), WatchOptions{OnReload: func(changed bool, err error) {
		if err == nil {
			reloads <- changed
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	src.set(nil, def(t, "a", "1.0.0", ""), def(t, "a", "1.1.0", ""))
	select {
	case changed := <-reloads:
		if !changed {
			t.Fatal("the reload saw no change")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no reload after a notification")
	}
	if res, _ := r.Resolve("a"); res.Version != "1.1.0" {
		t.Fatalf("resolved %s", res.Version)
	}
}

func TestWatchPollsDirectory(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.agent.md"), minimal("a", "1.0.0"))
	r := NewRegistry(DirSource(dir), Checks{})
	if _, err := r.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	changes := make(chan struct{}, 8)
	stop, err := r.Watch(context.Background(), WatchOptions{Interval: 20 * time.Millisecond, OnReload: func(changed bool, err error) {
		if changed && err == nil {
			changes <- struct{}{}
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	writeFile(t, filepath.Join(dir, "b.agent.md"), minimal("b", "1.0.0"))
	select {
	case <-changes:
	case <-time.After(5 * time.Second):
		t.Fatal("the poll never saw the new file")
	}
	if _, err := r.Resolve("b"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "b.agent.md")); err != nil {
		t.Fatal(err)
	}
}

// TestExamples loads the shipped example definitions, so they stay valid.
func TestExamples(t *testing.T) {
	r := NewRegistry(DirSource(filepath.Join("..", "..", "examples", "agents")), Checks{})
	if _, err := r.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	res, err := r.Resolve("repo-steward@^1")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Subagents) != 1 || res.Subagents[0].Agent.Name != "assistant" {
		t.Fatalf("got %+v", res.Subagents)
	}
	// The guide embeds each example with fsrc; keep it current.
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "agent-definitions.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range r.List() {
		if !strings.Contains(string(doc), string(d.Raw())) {
			t.Errorf("docs/agent-definitions.md does not embed %s as it is; run fsrc run docs/agent-definitions.md", d.Path)
		}
	}
}
