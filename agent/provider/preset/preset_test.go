package preset_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/provider"
	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/provider/preset"
	"github.com/urmzd/saige/agent/provider/split"
	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
)

// call is one request a fake adapter received, with the options it would
// put on the wire.
type call struct {
	model string
	opts  types.RequestOptions
}

// fake is a recording adapter built from one provider.Config.
type fake struct {
	cfg    provider.Config
	caps   types.ModelCapabilities
	script func(n int) (<-chan types.Delta, error)

	mu    *sync.Mutex
	calls *[]call
	n     *int
}

func (f *fake) Name() string                          { return string(f.cfg.Provider) }
func (f *fake) Model() string                         { return string(f.cfg.Model) }
func (f *fake) Capabilities() types.ModelCapabilities { return f.caps }
func (f *fake) EffectiveOptions() types.RequestOptions {
	return f.cfg.Options.Clone()
}
func (f *fake) chatStream(ctx context.Context, m []types.Message, t []types.ToolDef) (<-chan types.Delta, error) {
	return f.Stream(ctx, types.Request{Messages: m, Tools: t, Options: &types.RequestOptions{}})
}

// Stream implements types.Provider.
func (f *fake) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	if req.Options != nil {
		return f.chatStreamWithOptions(ctx, req.Messages, req.Tools, *req.Options)
	}
	return f.chatStream(ctx, req.Messages, req.Tools)
}

// SupportsOptions implements types.OptionsProvider.
func (f *fake) SupportsOptions() bool { return true }
func (f *fake) chatStreamWithOptions(_ context.Context, _ []types.Message, _ []types.ToolDef, o types.RequestOptions) (<-chan types.Delta, error) {
	f.mu.Lock()
	*f.calls = append(*f.calls, call{model: string(f.cfg.Model), opts: f.cfg.Options.Merge(o)})
	n := *f.n
	*f.n++
	f.mu.Unlock()
	return f.script(n)
}

// recorder builds fakes and keeps every config and call.
type recorder struct {
	mu      sync.Mutex
	configs map[string]provider.Config
	calls   []call
	scripts map[string]func(n int) (<-chan types.Delta, error)
}

func newRecorder() *recorder {
	return &recorder{configs: map[string]provider.Config{}, scripts: map[string]func(int) (<-chan types.Delta, error){}}
}

func (r *recorder) factory(_ context.Context, cfg provider.Config) (types.Provider, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.configs[string(cfg.Model)] = cfg
	script := r.scripts[string(cfg.Model)]
	if script == nil {
		script = func(int) (<-chan types.Delta, error) { return text("ok " + string(cfg.Model)), nil }
	}
	caps, _ := catalog.Lookup(cfg.Provider, cfg.Model)
	return &fake{cfg: cfg, caps: caps, script: script, mu: &r.mu, calls: &r.calls, n: new(int)}, nil
}

func text(s string) <-chan types.Delta {
	ds := agenttest.TextResponse(s)
	ch := make(chan types.Delta, len(ds))
	for _, d := range ds {
		ch <- d
	}
	close(ch)
	return ch
}

func fails(kind types.ErrorKind) func(int) (<-chan types.Delta, error) {
	return func(int) (<-chan types.Delta, error) {
		return nil, &types.ProviderError{Kind: kind, Err: errors.New(kind.String())}
	}
}

func everyone(string) string { return "key" }

func overlay(t *testing.T, doc string) *catalog.Catalog {
	t.Helper()
	layer, err := catalog.Load(strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	c, err := catalog.Merge(catalog.Default(), layer)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const chainDoc = `{"version":1,"presets":{
 "p":{"options":{"temperature":0.4,"max_output_tokens":2000},
  "retry":{"max_attempts":3,"base_delay":"1ms","max_delay":"1ms"},
  "chain":[
   {"id":"a","provider":"openai","model":"gpt-4.1","options":{"seed":1}},
   {"id":"b","provider":"openai","model":"gpt-4o","options":{"temperature":0.9,"seed":2}},
   {"id":"c","provider":"google","model":"gemini-2.5-flash","options":{"seed":3,"reasoning":{"budget":1024}}}]},
 "q":{"chain":[{"id":"x","provider":"anthropic","model":"claude-3-5-haiku","options":{"temperature":0.1}}]}}}`

func drain(t *testing.T) func(<-chan types.Delta, error) ([]types.RouteDelta, string) {
	return func(ch <-chan types.Delta, err error) ([]types.RouteDelta, string) { return read(t, ch, err) }
}

func read(t *testing.T, ch <-chan types.Delta, err error) ([]types.RouteDelta, string) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	var routes []types.RouteDelta
	var out string
	for d := range ch {
		switch v := d.(type) {
		case types.RouteDelta:
			routes = append(routes, v)
		case types.PartDelta:
			out += v.Text
		case types.ErrorDelta:
			t.Fatal(v.Error)
		}
	}
	return routes, out
}

func TestFallbackConsistencyRecording(t *testing.T) {
	cat := overlay(t, chainDoc)
	rec := newRecorder()
	rec.scripts["gpt-4.1"] = fails(types.ErrorKindTransient)
	rec.scripts["gpt-4o"] = fails(types.ErrorKindContextLength)
	b, err := preset.Build(context.Background(), cat, "p", nil, preset.Options{Getenv: everyone, Factory: rec.factory})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	rp, _ := b.Resolved("p")
	for _, e := range rp.Chain {
		if got := rec.configs[string(e.Model)].Options; !reflect.DeepEqual(got, e.Options) {
			t.Fatalf("%s built with %+v, resolved %+v", e.ID, got, e.Options)
		}
	}
	none := types.RequestOptions{ToolChoice: &types.ToolChoice{Mode: types.ToolChoiceNone}}
	routes, out := drain(t)(b.Session().(types.OptionsProvider).Stream(context.Background(), types.Request{Options: &none}))
	if out != "ok gemini-2.5-flash" {
		t.Fatalf("served %q", out)
	}
	if len(routes) != 3 || routes[2].Reason != "context_length" {
		t.Fatalf("routes %+v", routes)
	}
	for i, r := range routes {
		e := rp.Chain[i]
		want := e.Options.Merge(none)
		if r.ConfigHash != e.ConfigHash || r.Preset != "p" || r.Options == nil || !reflect.DeepEqual(*r.Options, want) {
			t.Fatalf("route %d: %+v, want hash %s options %+v", i, r, e.ConfigHash, want)
		}
	}
	perModel := map[string]int{}
	for _, c := range rec.calls {
		perModel[c.model]++
		e := entryFor(rp, c.model)
		if c.opts.ToolChoice == nil || c.opts.ToolChoice.Mode != types.ToolChoiceNone {
			t.Fatalf("%s lost the request override", c.model)
		}
		if !reflect.DeepEqual(c.opts.Temperature, e.Options.Temperature) || !reflect.DeepEqual(c.opts.Seed, e.Options.Seed) {
			t.Fatalf("%s saw another entry's sampling: %+v", c.model, c.opts)
		}
	}
	if perModel["gpt-4.1"] != 3 || perModel["gpt-4o"] != 1 || perModel["gemini-2.5-flash"] != 1 {
		t.Fatalf("attempts %v", perModel)
	}
	group, profile := b.ConfigKey(types.PresetTarget("p")), b.ConfigKey(types.ProfileTarget(rp.Chain[0].ProfileID))
	if group == "" || profile == "" || group == profile || b.ConfigKey(types.PresetTarget("nope")) != "" ||
		b.ConfigKey(types.ProfileTarget("nope")) != "" || b.ConfigKey(types.ModelTarget("gpt-4.1")) != "" {
		t.Fatal("config keys")
	}
}

func entryFor(rp catalog.ResolvedPreset, model string) catalog.ResolvedEntry {
	for _, e := range rp.Chain {
		if string(e.Model) == model {
			return e
		}
	}
	return catalog.ResolvedEntry{}
}

func TestOptionalEntryDropped(t *testing.T) {
	cat := overlay(t, `{"version":1,"presets":{"p":{"chain":[
		{"provider":"openai","model":"gpt-4.1","api_key_env":"ABSENT_KEY","optional":true},
		{"provider":"ollama","model":"qwen3"}]},
		"strict":{"chain":[{"provider":"openai","model":"gpt-4.1","api_key_env":"ABSENT_KEY"}]}}}`)
	env := func(string) string { return "" }
	rec := newRecorder()
	b, err := preset.Build(context.Background(), cat, "p", nil, preset.Options{Getenv: env, Factory: rec.factory})
	if err != nil {
		t.Fatal(err)
	}
	rp, _ := b.Resolved("p")
	if len(rp.Chain) != 1 || rp.Chain[0].Provider != "ollama" || len(b.Warnings()) == 0 {
		t.Fatalf("chain %+v warnings %v", rp.Chain, b.Warnings())
	}
	if _, ok := rec.configs["gpt-4.1"]; ok {
		t.Fatal("dropped entry was built")
	}
	_, err = preset.Build(context.Background(), cat, "strict", nil, preset.Options{Getenv: env, Factory: rec.factory})
	if !types.IsAuth(err) || !strings.Contains(err.Error(), "ABSENT_KEY") {
		t.Fatalf("got %v", err)
	}
}

func TestBuildRejectsInvalidPreset(t *testing.T) {
	cat := overlay(t, `{"version":1}`)
	if _, err := preset.Build(context.Background(), cat, "missing", nil, preset.Options{}); !errors.Is(err, catalog.ErrInvalidCatalog) {
		t.Fatalf("got %v", err)
	}
}

func TestModelReferenceBuildsOneEntry(t *testing.T) {
	rec := newRecorder()
	b, err := preset.Build(context.Background(), catalog.Default(), "openai/gpt-4o", nil, preset.Options{Getenv: everyone, Factory: rec.factory})
	if err != nil {
		t.Fatal(err)
	}
	routes, out := drain(t)(b.Session().Stream(context.Background(), types.Request{}))
	if out != "ok gpt-4o" || routes[0].Profile != "openai/gpt-4o" {
		t.Fatalf("%q %+v", out, routes)
	}
}

func TestGroupPinViaConfigContent(t *testing.T) {
	cat := overlay(t, chainDoc)
	b, err := preset.Build(context.Background(), cat, "p", []types.PresetName{"q"}, preset.Options{Getenv: everyone, Factory: newRecorder().factory})
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.NewAgent(agent.AgentConfig{SystemPrompt: "sys"}, agent.WithPreset(b))
	run := func(msg types.Message) []types.RouteDelta {
		t.Helper()
		stream := ag.Invoke(context.Background(), []types.Message{msg})
		var routes []types.RouteDelta
		for _, d := range agenttest.CollectDeltas(stream.Deltas()) {
			if r, ok := d.(types.RouteDelta); ok {
				routes = append(routes, r)
			}
		}
		if err := stream.Wait(); err != nil {
			t.Fatal(err)
		}
		return routes
	}
	if r := run(types.UserMsg(types.Text("hi"))); r[len(r)-1].Preset != "p" {
		t.Fatalf("first turn %+v", r)
	}
	r := run(types.UserMessage{Parts: []types.UserPart{types.TextPart{Text: "extract"}, types.ConfigPart{Target: types.PresetTarget("q")}}})
	last := r[len(r)-1]
	if last.Preset != "q" || last.Profile != "q/x" || last.Reason != "pinned" {
		t.Fatalf("group pin %+v", last)
	}
	// The committed turn records the configuration that produced it.
	msgs, err := ag.Tree().FlattenBranch(ag.Tree().Active())
	if err != nil {
		t.Fatal(err)
	}
	var rc *types.RoutePart
	for _, m := range msgs {
		if am, ok := m.(types.AssistantMessage); ok {
			for _, c := range am.Parts {
				if v, ok := c.(types.RoutePart); ok {
					rc = &v
				}
			}
		}
	}
	if rc == nil || rc.Preset != "q" || rc.ConfigHash != last.ConfigHash || rc.Options == nil {
		t.Fatalf("route content %+v", rc)
	}
	raw, err := tree.MarshalMessage(types.AssistantMessage{Parts: []types.AssistantPart{*rc}})
	if err != nil {
		t.Fatal(err)
	}
	back, err := tree.UnmarshalMessage(types.RoleAssistant, raw)
	if err != nil || !reflect.DeepEqual(back.(types.AssistantMessage).Parts[0], *rc) {
		t.Fatalf("tree round trip: %v %#v", err, back)
	}
}

func TestHardLockOutranksGroupPin(t *testing.T) {
	cat := overlay(t, chainDoc)
	b, err := preset.Build(context.Background(), cat, "p", []types.PresetName{"q"}, preset.Options{Getenv: everyone, Factory: newRecorder().factory})
	if err != nil {
		t.Fatal(err)
	}
	s := b.Session()
	routes, _ := drain(t)(s.Stream(context.Background(), types.Request{}))
	first := routes[0].Profile
	loop := []types.Message{
		types.AssistantMessage{Parts: []types.AssistantPart{
			types.ThinkingPart{Text: "t", Signature: "sig"}, types.ToolCallPart{ID: "c1", Name: "f"}}},
		types.UserMessage{Parts: []types.UserPart{types.ToolResultPart{CallID: "c1", Parts: []types.ToolOutputPart{types.Text("r")}}}},
	}
	pinned, err := s.(types.TargetSwitcher).WithTarget(types.PresetTarget("q"))
	if err != nil {
		t.Fatal(err)
	}
	routes, _ = drain(t)(pinned.Stream(context.Background(), types.Request{Messages: loop}))
	if routes[0].Profile != first || routes[0].Reason != "locked" {
		t.Fatalf("hard lock lost: %+v", routes)
	}
}

func TestSplitArmsArePresets(t *testing.T) {
	cat := overlay(t, chainDoc)
	opts := preset.Options{Getenv: everyone, Factory: newRecorder().factory}
	arm, err := preset.Build(context.Background(), cat, "q", nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	other, err := preset.Build(context.Background(), cat, "p", nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	sp, err := split.New(split.Config{Experiment: "exp", Arms: []split.Arm{
		{Label: "q", Weight: 1, Provider: arm.Session()},
		{Label: "p", Weight: 0, Provider: other.Session()},
	}})
	if err != nil {
		t.Fatal(err)
	}
	routes, _ := drain(t)(sp.Stream(context.Background(), types.Request{}))
	rp, _ := arm.Resolved("q")
	last := routes[len(routes)-1]
	if last.Variant != "q" || last.Preset != "q" || last.ConfigHash != rp.Chain[0].ConfigHash {
		t.Fatalf("variant route %+v", last)
	}
}

func TestDefaultsCarryCompaction(t *testing.T) {
	cat := overlay(t, `{"version":1,"presets":{"p":{"compaction":{"strategy":"keep_recent","keep_turns":6},
		"chain":[{"provider":"openai","model":"gpt-4.1"}]}}}`)
	b, err := preset.Build(context.Background(), cat, "p", nil, preset.Options{Getenv: everyone, Factory: newRecorder().factory})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	cc := b.Defaults().Compaction
	if cc == nil || cc.Strategy != types.CompactKeepRecent || cc.KeepTurns != 6 {
		t.Fatalf("compaction = %+v", cc)
	}
}
