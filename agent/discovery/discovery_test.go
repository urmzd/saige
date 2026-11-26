package discovery_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/discovery"
	"github.com/urmzd/saige/agent/types"
)

func defs(names ...string) []types.ToolDef {
	out := make([]types.ToolDef, 0, len(names))
	for _, n := range names {
		out = append(out, types.ToolDef{Name: n, Description: "The " + strings.ReplaceAll(n, "_", " ") + " tool.",
			Parameters: types.ParameterSchema{Type: types.SchemaObject, Properties: map[string]types.PropertyDef{
				"q": {Type: types.SchemaString, Description: "what " + n + " takes"},
			}}})
	}
	return out
}

func scope(conv string) context.Context {
	return agent.WithRunScope(context.Background(), agent.RunScope{Agent: "a", Conversation: types.NodeID(conv)})
}

func call(t *testing.T, d *discovery.Discovery, name string, ctx context.Context, args map[string]any) string {
	t.Helper()
	for _, tool := range d.Tools() {
		if tool.Definition().Name == name {
			out, err := tool.Execute(ctx, args)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			return out
		}
	}
	t.Fatalf("no tool %s", name)
	return ""
}

func callErr(d *discovery.Discovery, name string, ctx context.Context, args map[string]any) error {
	for _, tool := range d.Tools() {
		if tool.Definition().Name == name {
			_, err := tool.Execute(ctx, args)
			return err
		}
	}
	return nil
}

func held(names ...string) func(string) bool {
	return func(n string) bool { return slices.Contains(names, n) }
}

type matches struct {
	Matches []struct {
		Kind    discovery.Kind `json:"kind"`
		Name    string         `json:"name"`
		Excerpt string         `json:"excerpt"`
	} `json:"matches"`
}

func found(t *testing.T, out string) []string {
	t.Helper()
	if !strings.HasPrefix(out, "{") {
		return nil
	}
	var m matches
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	var names []string
	for _, x := range m.Matches {
		names = append(names, string(x.Kind)+":"+x.Name)
	}
	return names
}

func TestPolicyHoldsToolsUntilLoaded(t *testing.T) {
	d := discovery.New("a", held("github_search", "github_comment"))
	tools := append(defs("read_file", "github_search", "github_comment"), d.Tools()[0].Definition(), d.Tools()[1].Definition())
	policy := d.Policy(nil)
	ctx := scope("c1")
	got, err := policy.Select(ctx, "a", tools)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"read_file", discovery.DiscoverName, discovery.LoadName}) {
		t.Fatalf("offered %v", got)
	}
	if out := call(t, d, discovery.LoadName, ctx, map[string]any{"kind": "tool", "name": "github_search"}); !strings.Contains(out, `"q"`) {
		t.Fatalf("load answered %s", out)
	}
	got, _ = policy.Select(ctx, "a", tools)
	if !slices.Contains(got, "github_search") || slices.Contains(got, "github_comment") {
		t.Fatalf("after load %v", got)
	}
	// Another conversation has loaded nothing.
	if got, _ := policy.Select(scope("c2"), "a", tools); slices.Contains(got, "github_search") {
		t.Fatalf("other conversation %v", got)
	}
	loaded, err := d.Loaded(context.Background(), agent.RunScope{Agent: "a", Conversation: "c1"})
	if err != nil || !slices.Equal(loaded, []string{"github_search"}) {
		t.Fatalf("loaded %v %v", loaded, err)
	}
}

// A durable store keeps what a conversation loaded across Discovery values,
// as a run resumed in another process has.
func TestStoreKeepsLoadsAcrossProcesses(t *testing.T) {
	store := &discovery.MemoryStore{}
	tools := defs("read_file", "deploy")
	first := &discovery.Discovery{Hold: held("deploy"), Store: store}
	ctx := scope("c1")
	if _, err := first.Policy(nil).Select(ctx, "a", tools); err != nil {
		t.Fatal(err)
	}
	call(t, first, discovery.LoadName, ctx, map[string]any{"kind": "tool", "name": "deploy"})

	resumed := &discovery.Discovery{Hold: held("deploy"), Store: store}
	got, err := resumed.Policy(nil).Select(ctx, "a", tools)
	if err != nil || !slices.Contains(got, "deploy") {
		t.Fatalf("resumed %v %v", got, err)
	}
}

func TestDiscoverRanksByTextAcrossKinds(t *testing.T) {
	d := discovery.New("a", held("deploy"),
		discovery.Entry{Kind: discovery.KindSkill, Name: "release-notes", Description: "Write release notes.", Text: "Group changes by area. Mention breaking changes first."},
		discovery.Entry{Kind: discovery.KindAgent, Name: "reviewer", Description: "Reviews diffs.", Text: "You review pull requests. Flag flaky tests and missing coverage."},
		discovery.Entry{Kind: discovery.KindAgent, Name: "writer", Description: "Writes docs.", Text: "You write user guides."},
	)
	ctx := scope("c1")
	if _, err := d.Policy(nil).Select(ctx, "a", defs("deploy")); err != nil {
		t.Fatal(err)
	}
	// The prompt, not only the name, finds an entry.
	if got := found(t, call(t, d, discovery.DiscoverName, ctx, map[string]any{"query": "flaky tests"})); len(got) == 0 || got[0] != "agent:reviewer" {
		t.Fatalf("flaky tests: %v", got)
	}
	if got := found(t, call(t, d, discovery.DiscoverName, ctx, map[string]any{"query": "breaking changes"})); len(got) == 0 || got[0] != "skill:release-notes" {
		t.Fatalf("breaking changes: %v", got)
	}
	// A held tool is found by its name, description and parameters.
	if got := found(t, call(t, d, discovery.DiscoverName, ctx, map[string]any{"query": "deploy"})); !slices.Contains(got, "tool:deploy") {
		t.Fatalf("deploy: %v", got)
	}
	// An empty query lists one kind, sorted.
	if got := found(t, call(t, d, discovery.DiscoverName, ctx, map[string]any{"query": "", "kind": "agent"})); !slices.Equal(got, []string{"agent:reviewer", "agent:writer"}) {
		t.Fatalf("list agents: %v", got)
	}
	if got := found(t, call(t, d, discovery.DiscoverName, ctx, map[string]any{"query": "", "k": 1})); len(got) != 1 {
		t.Fatalf("k=1: %v", got)
	}
	if out := call(t, d, discovery.DiscoverName, ctx, map[string]any{"query": "zebra"}); !strings.Contains(out, "Nothing matches") {
		t.Fatalf("no match: %s", out)
	}
	// A loaded tool is no longer something to discover.
	call(t, d, discovery.LoadName, ctx, map[string]any{"kind": "tool", "name": "deploy"})
	if got := found(t, call(t, d, discovery.DiscoverName, ctx, map[string]any{"query": "", "kind": "tool"})); len(got) != 0 {
		t.Fatalf("loaded tool still listed: %v", got)
	}
}

func TestLoadEntries(t *testing.T) {
	loaded := ""
	d := discovery.New("a", nil,
		discovery.Entry{Kind: discovery.KindSkill, Name: "go-style", Description: "Go style.", Text: "Use gofmt.",
			Load: func(context.Context) (string, error) { loaded = "go-style"; return "SKILL TEXT", nil }},
		discovery.Entry{Kind: discovery.KindAgent, Name: "reviewer", Description: "Reviews diffs.", Text: "You review."},
	)
	ctx := scope("c1")
	if out := call(t, d, discovery.LoadName, ctx, map[string]any{"kind": "skill", "name": "go-style"}); out != "SKILL TEXT" || loaded != "go-style" {
		t.Fatalf("skill: %q %q", out, loaded)
	}
	if out := call(t, d, discovery.LoadName, ctx, map[string]any{"kind": "agent", "name": "reviewer"}); !strings.Contains(out, "Reviews diffs.") || !strings.Contains(out, "You review.") {
		t.Fatalf("agent: %q", out)
	}
	for _, args := range []map[string]any{
		{"kind": "agent", "name": "go-style"},
		{"kind": "tool", "name": "ghost"},
		{"kind": "planet", "name": "x"},
		{"name": "reviewer"},
		{"kind": "agent"},
	} {
		if err := callErr(d, discovery.LoadName, ctx, args); err == nil {
			t.Errorf("load %v succeeded", args)
		}
	}
}

// discover never reveals a tool another policy hides, and discover and load
// stay offered whatever the inner policy narrows.
func TestInnerPolicyNarrows(t *testing.T) {
	d := discovery.New("a", held("secret", "visible"))
	tools := append(defs("read_file", "secret", "visible"), d.Tools()[0].Definition(), d.Tools()[1].Definition())
	inner := agent.ToolPolicyFunc(func(_ context.Context, _ string, defs []types.ToolDef) ([]string, error) {
		return []string{"read_file", "visible"}, nil
	})
	ctx := scope("c1")
	got, err := d.Policy(inner).Select(ctx, "a", tools)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"read_file", discovery.DiscoverName, discovery.LoadName}) {
		t.Fatalf("offered %v", got)
	}
	if got := found(t, call(t, d, discovery.DiscoverName, ctx, map[string]any{"query": "", "kind": "tool"})); !slices.Equal(got, []string{"tool:visible"}) {
		t.Fatalf("discover %v", got)
	}
}

// Lowercasing can change a string's byte length; the excerpt still finds
// the match.
func TestExcerptFindsTheMatchAfterWideRunes(t *testing.T) {
	text := strings.Repeat("İ", 400) + " needle " + strings.Repeat("x", 400)
	d := discovery.New("a", nil, discovery.Entry{Kind: discovery.KindAgent, Name: "ops", Text: text})
	var m matches
	if err := json.Unmarshal([]byte(call(t, d, discovery.DiscoverName, scope("c1"), map[string]any{"query": "needle"})), &m); err != nil {
		t.Fatal(err)
	}
	if ex := m.Matches[0].Excerpt; !strings.Contains(ex, "needle") {
		t.Fatalf("excerpt %q", ex)
	}
}

func TestExcerptCentersOnTheMatch(t *testing.T) {
	text := strings.Repeat("filler words here. ", 40) + "The rollback procedure needs two approvals. " + strings.Repeat("more filler. ", 40)
	d := discovery.New("a", nil, discovery.Entry{Kind: discovery.KindAgent, Name: "ops", Text: text})
	var m matches
	if err := json.Unmarshal([]byte(call(t, d, discovery.DiscoverName, scope("c1"), map[string]any{"query": "rollback"})), &m); err != nil {
		t.Fatal(err)
	}
	ex := m.Matches[0].Excerpt
	if !strings.Contains(ex, "rollback procedure") || !strings.HasPrefix(ex, "…") || !strings.HasSuffix(ex, "…") || len([]rune(ex)) > discovery.ExcerptLen+2 {
		t.Fatalf("excerpt %q", ex)
	}
}

// The store forgets the conversation used longest ago, not the one that
// loaded first.
func TestMemoryStoreForgetsLeastRecentlyUsed(t *testing.T) {
	s := &discovery.MemoryStore{MaxScopes: 2}
	ctx := context.Background()
	for _, scope := range []string{"a", "b"} {
		if err := s.Add(ctx, scope, "t"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Loaded(ctx, "a"); err != nil { // a is in use again
		t.Fatal(err)
	}
	if err := s.Add(ctx, "c", "t"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Loaded(ctx, "b"); len(got) != 0 {
		t.Fatalf("least recently used kept: %v", got)
	}
	for _, scope := range []string{"a", "c"} {
		if got, _ := s.Loaded(ctx, scope); !slices.Equal(got, []string{"t"}) {
			t.Fatalf("%s: %v", scope, got)
		}
	}
}

// A sub-agent inherits its parent's ToolPolicy. The parent's Discovery
// passes the sub-agent's turns through, so the parent's holds stay its own.
func TestPolicyPassesOtherAgentsThrough(t *testing.T) {
	d := discovery.New("lead", held("grep"))
	tools := defs("read_file", "grep")
	ctx := agent.WithRunScope(context.Background(), agent.RunScope{Agent: "worker", Conversation: "c"})
	got, err := d.Policy(nil).Select(ctx, "worker", tools)
	if err != nil || !slices.Equal(got, []string{"read_file", "grep"}) {
		t.Fatalf("worker offered %v %v", got, err)
	}
	ctx = agent.WithRunScope(context.Background(), agent.RunScope{Agent: "lead", Conversation: "c"})
	if got, _ := d.Policy(nil).Select(ctx, "lead", tools); slices.Contains(got, "grep") {
		t.Fatalf("lead offered %v", got)
	}
}

// Outside an agent loop the policy and the tools use the same key.
func TestNoRunScopeSharesTheOwnersKey(t *testing.T) {
	d := discovery.New("a", held("deploy"))
	ctx := context.Background()
	if _, err := d.Policy(nil).Select(ctx, "a", defs("deploy")); err != nil {
		t.Fatal(err)
	}
	call(t, d, discovery.LoadName, ctx, map[string]any{"kind": "tool", "name": "deploy"})
	if got, _ := d.Policy(nil).Select(ctx, "a", defs("deploy")); !slices.Contains(got, "deploy") {
		t.Fatalf("offered %v", got)
	}
}
