package bind

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/definition"
	"github.com/urmzd/saige/agent/mcp"
	"github.com/urmzd/saige/agent/memory"
	"github.com/urmzd/saige/agent/provider"
	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/provider/preset"
	"github.com/urmzd/saige/agent/skills"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/tools"
	"github.com/urmzd/saige/tools/exec"
)

// fakePreset serves a scripted provider.
type fakePreset struct{ p types.Provider }

func (f fakePreset) Provider() types.Provider       { return f.p }
func (f fakePreset) Defaults() types.PresetDefaults { return types.PresetDefaults{} }

// resolve loads definitions written as frontmatter lines and resolves ref.
func resolve(t *testing.T, ref string, files ...string) *definition.Resolved {
	t.Helper()
	var defs []*definition.Definition
	for _, f := range files {
		d, err := definition.Parse([]byte(f), "test")
		if err != nil {
			t.Fatal(err)
		}
		defs = append(defs, d)
	}
	r := definition.NewRegistry(definition.StaticSource(defs...), definition.Checks{})
	if _, err := r.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	res, err := r.Resolve(ref)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func file(name, extra, prompt string) string {
	return "---\napiVersion: saige/v1\nname: " + name + "\nversion: 1.0.0\n" + extra + "---\n" + prompt + "\n"
}

func bind(t *testing.T, res *definition.Resolved, env Env) *Bound {
	t.Helper()
	b, err := Bind(context.Background(), res, env)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// recorder is a preset factory that records the configurations it builds.
type recorder struct {
	mu      sync.Mutex
	configs []provider.Config
}

func (r *recorder) factory(_ context.Context, cfg provider.Config) (types.Provider, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.configs = append(r.configs, cfg)
	return &agenttest.ScriptedProvider{}, nil
}

func (r *recorder) models() []string {
	var out []string
	for _, c := range r.configs {
		out = append(out, c.Provider+"/"+c.Model)
	}
	return out
}

func modelEnv(rec *recorder, getenv func(string) string) Env {
	return Env{Catalog: catalog.Default(), PresetOptions: preset.Options{Getenv: getenv, Factory: rec.factory}}
}

func everyKey(string) string { return "key" }

func TestBindModel(t *testing.T) {
	cases := []struct {
		name, model string
		getenv      func(string) string
		want        []string
	}{
		{"provider/model", "model: anthropic/claude-haiku-5-5\n", everyKey, []string{"anthropic/claude-haiku-5-5"}},
		{"preset", "model: anthropic\n", everyKey, nil},
		{"fallback", "model:\n  use: anthropic/claude-haiku-5-5\n  fallback: [openai/gpt-6-luna]\n", everyKey,
			[]string{"anthropic/claude-haiku-5-5", "openai/gpt-6-luna"}},
		{"fallback without credentials is dropped", "model:\n  use: anthropic/claude-haiku-5-5\n  fallback: [openai/gpt-6-luna]\n",
			func(k string) string {
				if k == "ANTHROPIC_API_KEY" {
					return "key"
				}
				return ""
			}, []string{"anthropic/claude-haiku-5-5"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			b := bind(t, resolve(t, "a", file("a", tc.model, "p")), modelEnv(rec, tc.getenv))
			if tc.want != nil && !slices.Equal(rec.models(), tc.want) {
				t.Fatalf("built %v, want %v", rec.models(), tc.want)
			}
			if len(rec.configs) == 0 || len(b.Options) == 0 {
				t.Fatal("no model was built")
			}
		})
	}

	// A preset fallback chain extends the preset's own chain.
	rec := &recorder{}
	bind(t, resolve(t, "a", file("a", "model:\n  use: anthropic\n  fallback: [openai/gpt-6-luna]\n", "p")), modelEnv(rec, everyKey))
	if got := rec.models(); len(got) < 2 || got[len(got)-1] != "openai/gpt-6-luna" {
		t.Fatalf("built %v", got)
	}

	// The host's preset overrides the root's model, not a sub-agent's.
	rec = &recorder{}
	env := modelEnv(rec, everyKey)
	env.Preset = fakePreset{&agenttest.ScriptedProvider{}}
	bind(t, resolve(t, "a",
		file("a", "model: openai/gpt-6-luna\nsubagents: [b]\n", "p"),
		file("b", "model: anthropic/claude-haiku-5-5\n", "q")), env)
	if !slices.Equal(rec.models(), []string{"anthropic/claude-haiku-5-5"}) {
		t.Fatalf("built %v", rec.models())
	}

	_, err := Bind(context.Background(), resolve(t, "a", file("a", "", "p")), Env{})
	if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "no model") {
		t.Fatalf("got %v", err)
	}
	_, err = Bind(context.Background(), resolve(t, "a", file("a", "model: nowhere\n", "p")), modelEnv(&recorder{}, everyKey))
	if err == nil || !strings.Contains(err.Error(), "nowhere") {
		t.Fatalf("got %v", err)
	}
}

func TestModelCheck(t *testing.T) {
	check := ModelCheck(catalog.Default())
	for _, ok := range []string{"anthropic", "anthropic/claude-haiku-5-5", "vertex/gemini-3.1-flash-lite"} {
		if err := check(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"nope", "acme/model-x"} {
		if err := check(bad); err == nil {
			t.Errorf("%s: want an error", bad)
		}
	}
}

func toolEnv(t *testing.T) Env {
	return Env{
		Preset:  fakePreset{&agenttest.ScriptedProvider{}},
		Harness: tools.HarnessOptions{Root: t.TempDir(), Network: exec.NetworkAllow},
	}
}

func names(r *types.ToolRegistry) []string {
	if r == nil {
		return nil
	}
	return toolNames(r)
}

func TestBindTools(t *testing.T) {
	env := toolEnv(t)
	env.Tools = types.NewToolRegistry(&agenttest.MockTool{Def: types.ToolDef{Name: "rag_search"}})
	b := bind(t, resolve(t, "a", file("a", "tools:\n  harness: [read, exec]\n  registry: [rag_search]\n", "p")), env)
	got := names(b.Config.Tools)
	for _, want := range []string{"rag_search", "read_file", "grep", "execute_code"} {
		if !slices.Contains(got, want) {
			t.Fatalf("missing %s in %v", want, got)
		}
	}
	if slices.Contains(got, "write_file") || b.Config.Workspace == nil {
		t.Fatalf("tools %v workspace %v", got, b.Config.Workspace)
	}
	// Without an approval block the markers stay.
	code, _ := b.Config.Tools.Get("execute_code")
	if _, marked := code.(*types.MarkedTool); !marked {
		t.Fatal("execute_code lost its marker without an approval block")
	}

	_, err := Bind(context.Background(), resolve(t, "a", file("a", "tools:\n  registry: [kg_search]\n", "p")), env)
	if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "rag_search") {
		t.Fatalf("got %v", err)
	}
	env.Harness.Root = ""
	_, err = Bind(context.Background(), resolve(t, "a", file("a", "tools:\n  harness: [read]\n", "p")), env)
	if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "root") {
		t.Fatalf("got %v", err)
	}
}

func TestApprovalGate(t *testing.T) {
	env := toolEnv(t)
	env.Tools = types.NewToolRegistry(
		&agenttest.MockTool{Def: types.ToolDef{Name: "lookup", Capability: types.ToolCapabilityRead}},
		&agenttest.MockTool{Def: types.ToolDef{Name: "mystery"}},
		types.WithMarkers(&agenttest.MockTool{Def: types.ToolDef{Name: "publish", Capability: types.ToolCapabilityWrite}},
			types.Marker{Kind: "human_approval", Message: "Publishing requires approval"}),
	)
	approval := "approval:\n  allow: [\"Bash(git status:*)\", publish]\n  ask: [lookup]\n  deny: [\"Bash(rm:*)\"]\n" +
		"  capabilities:\n    unknown: deny\n  deny_after: 2\n  grant: tool\n"
	b := bind(t, resolve(t, "a",
		file("a", "tools:\n  harness: [read, write, exec]\n  registry: [lookup, mystery, publish]\n"+approval+"subagents: [helper]\n", "p"),
		file("helper", "", "h")), env)

	code, _ := b.Config.Tools.Get("execute_code")
	if _, marked := code.(*types.MarkedTool); marked {
		t.Fatal("the approval block did not take over execute_code's marker")
	}
	sh := func(cmd string) map[string]any { return map[string]any{"language": "shell", "code": cmd} }
	gate := b.Config.ToolGate
	def := func(name string) types.ToolDef {
		if tool, ok := b.Config.Tools.Get(name); ok {
			return tool.Definition()
		}
		return types.ToolDef{Name: name}
	}
	cases := []struct {
		tool string
		args map[string]any
		want types.GateOutcome
	}{
		{"execute_code", sh("git status --short"), types.GateAllow},
		{"execute_code", sh("git status && rm -rf ."), types.GateDeny},
		{"execute_code", sh("rm -rf ."), types.GateDeny},
		{"execute_code", sh("ls"), types.GateRequireApproval},                                          // its marker asks
		{"execute_code", map[string]any{"language": "python", "code": "1"}, types.GateRequireApproval}, // Bash rules skip Python
		{"read_file", map[string]any{"path": "x"}, types.GateAllow},                                    // read class
		{"write_file", map[string]any{"path": "x"}, types.GateRequireApproval},                         // write class
		{"lookup", nil, types.GateRequireApproval},                                                     // ask rule beats the read class
		{"mystery", nil, types.GateDeny},                                                               // unknown: deny
		{"publish", nil, types.GateAllow},                                                              // allow rule beats the marker
		{"delegate_to_helper", map[string]any{"task": "x"}, types.GateAllow},                           // delegation is free
	}
	for _, tc := range cases {
		if got := gate.Check(context.Background(), def(tc.tool), tc.args).Outcome; got != tc.want {
			t.Errorf("%s %v: got %v, want %v", tc.tool, tc.args, got, tc.want)
		}
	}
	if b.Config.ApprovalPolicy == nil || b.Config.ApprovalPolicy.DenyAfter != 2 {
		t.Fatalf("approval policy %+v", b.Config.ApprovalPolicy)
	}
	if err := b.CheckGrant(&types.GrantRequest{Scope: types.GrantSession}); !errors.Is(err, types.ErrInvalidGrant) {
		t.Fatalf("a session grant passed a tool limit: %v", err)
	}
	for _, ok := range []types.GrantScope{types.GrantOnce, types.GrantArgs, types.GrantTool} {
		if err := b.CheckGrant(&types.GrantRequest{Scope: ok}); err != nil {
			t.Fatalf("%s: %v", ok, err)
		}
	}
	if err := b.CheckGrant(nil); err != nil {
		t.Fatal(err)
	}

	// The host's gate still applies.
	env.ToolGate = types.DenyListGate("read_file")
	b = bind(t, resolve(t, "a", file("a", "tools:\n  harness: [read]\napproval:\n  allow: [read_file]\n", "p")), env)
	if got := b.Config.ToolGate.Check(context.Background(), def("read_file"), map[string]any{"path": "x"}).Outcome; got != types.GateDeny {
		t.Fatalf("host gate ignored: %v", got)
	}
}

// TestApprovalRun runs an agent whose model calls execute_code: an allowed
// command runs without a prompt and a denied one is refused, so an allow
// rule really replaces the marker.
func TestApprovalRun(t *testing.T) {
	model := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("c1", "execute_code", map[string]any{"language": "shell", "code": "echo allowed-ran"}),
		agenttest.ToolCallResponse("c2", "execute_code", map[string]any{"language": "shell", "code": "rm -rf ."}),
		agenttest.TextResponse("done"),
	}}
	env := toolEnv(t)
	env.Preset = fakePreset{model}
	b := bind(t, resolve(t, "a", file("a", "tools:\n  harness: [exec]\napproval:\n  allow: [\"Bash(echo:*)\"]\n  deny: [\"Bash(rm:*)\"]\n", "p")), env)
	stream := b.NewAgent().Invoke(context.Background(), []types.Message{types.NewUserMessage("go")})
	results := map[string]types.ToolExecEndDelta{}
	for d := range stream.Deltas() {
		switch v := d.(type) {
		case types.MarkerDelta:
			t.Fatalf("asked about %s", v.ToolName)
		case types.ToolExecEndDelta:
			results[v.ToolCallID] = v
		case types.ErrorDelta:
			t.Fatalf("run failed: %v", v.Error)
		}
	}
	if !strings.Contains(results["c1"].Result, "allowed-ran") {
		t.Fatalf("allowed call: %+v", results["c1"])
	}
	if !strings.Contains(results["c2"].Error+results["c2"].Result, "denied") {
		t.Fatalf("denied call: %+v", results["c2"])
	}
}

func skillCatalog(t *testing.T) *skills.Catalog {
	t.Helper()
	skill := func(name, body string) *fstest.MapFile {
		return &fstest.MapFile{Data: []byte("---\nname: " + name + "\ndescription: The " + name + " skill.\n---\n" + body + "\n")}
	}
	fsys := fstest.MapFS{
		"lazy-one/SKILL.md":  skill("lazy-one", "LAZY BODY"),
		"eager-one/SKILL.md": skill("eager-one", "EAGER BODY"),
		"pin-one/SKILL.md":   skill("pin-one", "PINNED BODY"),
		"hidden/SKILL.md":    skill("hidden", "SEARCH BODY"),
		"release/SKILL.md":   skill("release", "RELEASE BODY"),
	}
	cat, err := skills.NewCatalog(context.Background(), []skills.SkillSource{skills.NewFSSource("test", fsys, ".")})
	if err != nil {
		t.Fatal(err)
	}
	return cat
}

func TestBindSkills(t *testing.T) {
	cat := skillCatalog(t)
	pin, err := cat.Load(context.Background(), "", "pin-one")
	if err != nil {
		t.Fatal(err)
	}
	model := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("ok"), agenttest.TextResponse("ok")}}
	env := Env{Preset: fakePreset{model}, Skills: cat}
	spec := "skills:\n  - lazy-one\n  - name: eager-one\n    mode: eager\n  - name: pin-one\n    mode: pinned\n    hash: " + pin.Hash +
		"\n  - name: hidden\n    mode: search\n  - name: release\n    mode: trigger\n    triggers: [\"(?i)\\\\brelease\\\\b\"]\n"
	b := bind(t, resolve(t, "a", file("a", spec, "BASE PROMPT")), env)
	prompt := b.Config.SystemPrompt
	for _, want := range []string{"BASE PROMPT", "- lazy-one:", "- release:", "EAGER BODY", "PINNED BODY", skills.SearchSkillsName} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	for _, unwanted := range []string{"- hidden:", "SEARCH BODY", "LAZY BODY", "- eager-one:"} {
		if strings.Contains(prompt, unwanted) {
			t.Fatalf("prompt has %q:\n%s", unwanted, prompt)
		}
	}
	if got := names(b.Config.Tools); !slices.Contains(got, skills.LoadSkillName) || b.Config.ToolPolicy == nil {
		t.Fatalf("skill tools %v", got)
	}

	// A trigger puts the skill's instructions in front of a matching
	// message, and only a matching one.
	a := b.NewAgent()
	for _, msg := range []string{"cut the Release now", "hello"} {
		if _, err := agent.Collect(a.Invoke(context.Background(), []types.Message{types.NewUserMessage(msg)}), nil); err != nil {
			t.Fatal(err)
		}
	}
	calls := model.Requests()
	first := userText(lastUser(calls[0].Messages))
	second := userText(lastUser(calls[1].Messages))
	if !strings.Contains(first, "RELEASE BODY") || !strings.Contains(first, "cut the Release now") {
		t.Fatalf("matching message: %q", first)
	}
	if strings.Contains(second, "RELEASE BODY") {
		t.Fatalf("unmatched message got the skill: %q", second)
	}

	// A pinned skill whose snapshot changed fails to bind.
	bad := strings.Replace(spec, pin.Hash, "sha256:"+strings.Repeat("0", 64), 1)
	if _, err := Bind(context.Background(), resolve(t, "a", file("a", bad, "p")), env); err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("got %v", err)
	}
	if _, err := Bind(context.Background(), resolve(t, "a", file("a", "skills: [ghost]\n", "p")), env); !errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("got %v", err)
	}
	if _, err := Bind(context.Background(), resolve(t, "a", file("a", "skills: [lazy-one]\n", "p")), Env{Preset: env.Preset}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("got %v", err)
	}
	check := SkillCheck(cat)
	if check("lazy-one", "") != nil || check("ghost", "") == nil || check("pin-one", "sha256:x") == nil {
		t.Fatal("SkillCheck")
	}
}

func lastUser(msgs []types.Message) types.UserMessage {
	for i := len(msgs) - 1; i >= 0; i-- {
		if u, ok := msgs[i].(types.UserMessage); ok {
			return u
		}
	}
	return types.UserMessage{}
}

func TestBindMemory(t *testing.T) {
	store := memory.NewFixture(memory.Record{Scope: memory.Scope{Tenant: "t", Namespace: "notes"}, Kind: memory.KindSemantic,
		Content: "The deploy window is Tuesday.", CreatedAt: time.Now()})
	model := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("ok")}}
	env := Env{
		Preset:       fakePreset{model},
		MemoryStores: map[string]memory.Store{"team": store},
		MemoryScope: func(context.Context, string) (memory.Scope, error) {
			return memory.Scope{Tenant: "t"}, nil
		},
	}
	b := bind(t, resolve(t, "a", file("a", "memory:\n  store: team\n", "p")), env)
	if got := names(b.Config.Tools); !slices.Contains(got, memory.RecallToolName) || !slices.Contains(got, memory.RememberToolName) {
		t.Fatalf("tools %v", got)
	}
	b = bind(t, resolve(t, "a", file("a", "memory:\n  store: team\n  read_only: true\n", "p")), env)
	if got := names(b.Config.Tools); !slices.Equal(got, []string{memory.RecallToolName}) {
		t.Fatalf("read-only tools %v", got)
	}

	b = bind(t, resolve(t, "a", file("a", "memory:\n  store: team\n  recall: inject\n  namespace: notes\n", "p")), env)
	if _, err := agent.Collect(b.NewAgent().Invoke(context.Background(), []types.Message{types.NewUserMessage("when is the deploy window?")}), nil); err != nil {
		t.Fatal(err)
	}
	if got := userText(lastUser(model.Requests()[0].Messages)); !strings.Contains(got, "Tuesday") || !strings.Contains(got, "deploy window?") {
		t.Fatalf("injected message: %q", got)
	}

	if _, err := Bind(context.Background(), resolve(t, "a", file("a", "memory:\n  store: other\n", "p")), env); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("got %v", err)
	}
	env.MemoryScope = nil
	if _, err := Bind(context.Background(), resolve(t, "a", file("a", "memory:\n  store: team\n", "p")), env); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("got %v", err)
	}
}

func TestBindSubagents(t *testing.T) {
	rec := &recorder{}
	env := modelEnv(rec, everyKey)
	env.Preset = fakePreset{&agenttest.ScriptedProvider{}}
	env.Harness = tools.HarnessOptions{Root: t.TempDir()}
	res := resolve(t, "lead",
		file("lead", "subagents:\n  - ref: worker@^1\n    description: Do the work.\n    budget:\n      max_iterations: 3\n      timeout: 1m\n      max_cost: 0.5\n"+
			"  - ref: bg\n    mode: spawn\n  - ref: peer\n    mode: handoff\n    budget:\n      max_iterations: 2\n", "lead"),
		file("worker", "description: Works.\nmodel: anthropic/claude-haiku-5-5\ntools:\n  harness: [read]\n"+
			"approval:\n  deny: [read_file]\ncompaction:\n  strategy: keep_recent\n  keep_turns: 2\ndials:\n  creativity: deterministic\n"+
			"limits:\n  max_iterations: 9\n  tool_timeout: 5s\nguardrails:\n  output: [pii]\n", "WORKER PROMPT"),
		file("bg", "", "bg"),
		file("peer", "description: A peer.\ndials:\n  creativity: creative\n", "PEER PROMPT"))
	b := bind(t, res, env)
	if len(b.Config.SubAgents) != 2 {
		t.Fatalf("sub-agents %+v", b.Config.SubAgents)
	}
	w := b.Config.SubAgents[0]
	if w.Name != "worker" || w.Description != "Do the work." || w.SystemPrompt != "WORKER PROMPT" || w.MaxIter != 3 ||
		w.Timeout != time.Minute || w.Provider == nil || !slices.Contains(names(w.Tools), "read_file") {
		t.Fatalf("worker %+v", w)
	}
	if bg := b.Config.SubAgents[1]; bg.Mode != agent.SubAgentSpawn || bg.Provider != nil || bg.MaxIter != 0 {
		t.Fatalf("bg %+v", bg)
	}
	if !slices.Equal(rec.models(), []string{"anthropic/claude-haiku-5-5"}) {
		t.Fatalf("models %v", rec.models())
	}

	// The worker's options replace what its definition declares on top of
	// the inherited config.
	parent := agent.AgentConfig{ToolGate: types.AllowAllGate{}, LLMTimeout: time.Hour, Budget: types.NewBudget(types.BudgetPolicy{})}
	cfg := parent
	for _, o := range w.Options {
		o(&cfg)
	}
	if cfg.CompactCfg == nil || cfg.CompactCfg.Strategy != types.CompactKeepRecent || cfg.ToolTimeout != 5*time.Second ||
		cfg.LLMTimeout != time.Hour || *cfg.Dials.Creativity != types.CreativityDeterministic || len(cfg.OutputGuardrails) != 1 ||
		cfg.Budget == parent.Budget || cfg.Budget.Policy().Limit != types.USD(0.5) {
		t.Fatalf("worker config %+v", cfg)
	}
	readDef, _ := w.Tools.Get("read_file")
	if got := cfg.ToolGate.Check(context.Background(), readDef.Definition(), map[string]any{"path": "x"}).Outcome; got != types.GateDeny {
		t.Fatalf("the worker's own approval rules do not apply: %v", got)
	}

	// The handoff member arrives through an option.
	var full agent.AgentConfig
	for _, o := range b.Options {
		o(&full)
	}
	if len(full.Handoffs) != 1 || full.Handoffs[0].Name != "peer" || full.Handoffs[0].MaxIter != 2 ||
		full.Handoffs[0].SystemPrompt != "PEER PROMPT" || *full.Handoffs[0].Dials.Creativity != types.CreativityCreative {
		t.Fatalf("handoffs %+v", full.Handoffs)
	}

	// A handoff member cannot carry its own run policy.
	_, err := Bind(context.Background(), resolve(t, "lead",
		file("lead", "subagents:\n  - ref: peer\n    mode: handoff\n", "lead"),
		file("peer", "approval:\n  deny: [x]\ncompaction:\n  strategy: none\n", "p")), env)
	if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "approval, compaction") {
		t.Fatalf("got %v", err)
	}
}

func TestBindLimitsGuardrailsCompaction(t *testing.T) {
	model := &agenttest.ScriptedProvider{}
	env := Env{Preset: fakePreset{model}}
	b := bind(t, resolve(t, "a", file("a",
		"dials:\n  creativity: focused\ncompaction:\n  strategy: sliding_window\n  window_size: 30\n"+
			"guardrails:\n  input:\n    - pii\n    - name: max_length\n      max: 100\n      parallel: true\n  output:\n"+
			"    - name: regex\n      redact: true\n      patterns:\n        - label: TICKET\n          expr: \"T-[0-9]+\"\n    - name: classifier\n      policy: Be kind.\n"+
			"limits:\n  max_iterations: 7\n  llm_timeout: 20s\n  budget:\n    max_cost: 2\n    max_requests: 9\n    warn_at: 0.5\n    on_exceed: ask\n", "p")), env)
	c := b.Config
	if c.MaxIter != 7 || c.LLMTimeout != 20*time.Second || c.CompactCfg.WindowSize != 30 || *c.Dials.Creativity != types.CreativityFocused {
		t.Fatalf("config %+v", c)
	}
	pol := c.Budget.Policy()
	if pol.Limit != types.USD(2) || pol.MaxRequests != 9 || pol.WarnAt != 0.5 || pol.OnExceed != types.BudgetRequireApproval {
		t.Fatalf("budget %+v", pol)
	}
	if len(c.InputGuardrails) != 2 || c.InputGuardrails[1].Mode != agent.GuardrailParallel || len(c.OutputGuardrails) != 2 ||
		c.OutputGuardrails[1].Name() != "classifier" {
		t.Fatalf("guardrails %+v %+v", c.InputGuardrails, c.OutputGuardrails)
	}
	if b.Pin().Name != "a" || b.Pin().Digest != b.Resolved.Digest || !strings.Contains(b.Pin().String(), "a@1.0.0") {
		t.Fatalf("pin %+v", b.Pin())
	}

	// A sub-agent classifier without a model of its own has nothing to run on.
	_, err := Bind(context.Background(), resolve(t, "a", file("a", "subagents: [b]\n", "p"),
		file("b", "guardrails:\n  output:\n    - name: classifier\n      policy: x\n", "q")), env)
	if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "classifier") {
		t.Fatalf("got %v", err)
	}
}

func TestBindMCP(t *testing.T) {
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "test", Version: "1"}, nil)
	schema := json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`)
	for _, name := range []string{"echo", "shout"} {
		server.AddTool(&mcpsdk.Tool{Name: name, InputSchema: schema}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: name}}}, nil
		})
	}
	srv := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil))
	// Registered first so it runs last, after each Bound closes its
	// sessions.
	t.Cleanup(srv.Close)
	env := Env{Preset: fakePreset{&agenttest.ScriptedProvider{}}, MCPServers: map[string]mcp.ServerSpec{"demo": mcp.Remote("demo", srv.URL)}}

	b := bind(t, resolve(t, "a", file("a", "tools:\n  mcp:\n    - server: demo\n      allow: [echo]\n", "p")), env)
	if got := names(b.Config.Tools); !slices.Equal(got, []string{"demo_echo"}) {
		t.Fatalf("tools %v", got)
	}
	tool, _ := b.Config.Tools.Get("demo_echo")
	out, err := tool.Execute(context.Background(), map[string]any{"text": "x"})
	if err != nil || !strings.Contains(out, "echo") {
		t.Fatalf("call: %q %v", out, err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	b = bind(t, resolve(t, "a", file("a", "tools:\n  mcp: [demo]\n", "p")), env)
	if got := names(b.Config.Tools); !slices.Equal(got, []string{"demo_echo", "demo_shout"}) {
		t.Fatalf("tools %v", got)
	}

	if _, err := Bind(context.Background(), resolve(t, "a", file("a", "tools:\n  mcp: [ghost]\n", "p")), env); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("got %v", err)
	}
	narrowed := env
	narrowed.MCPServers = map[string]mcp.ServerSpec{"demo": {Name: "demo", URL: srv.URL, AllowedTools: []string{"echo"}}}
	if _, err := Bind(context.Background(), resolve(t, "a", file("a", "tools:\n  mcp:\n    - server: demo\n      allow: [shout]\n", "p")), narrowed); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("a definition widened the host's allow list: %v", err)
	}
}
