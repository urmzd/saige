package agent

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

// cheapScripted is a priced ScriptedProvider with its own model name, so a
// test can tell summary calls from turns in the budget.
type cheapScripted struct {
	*agenttest.ScriptedProvider
}

func (cheapScripted) Model() string { return "cheap-model" }
func (cheapScripted) Capabilities() types.ModelCapabilities {
	return types.ModelCapabilities{
		Provider: "scripted",
		Model:    "cheap-model",
		Pricing:  types.Pricing{InputPerMTok: 0.1, OutputPerMTok: 0.1, AsOf: "2026-07-01"},
	}
}

// twoToolTurns is a task and two tool turns: with one turn kept, the first
// turn is the older span.
func twoToolTurns() []types.Message {
	return []types.Message{
		types.UserMsg(types.Text("go")),
		types.AssistantMessage{Parts: []types.AssistantPart{types.ToolCallPart{ID: "a", Name: "t"}}},
		types.ToolResults(types.ToolResultPart{CallID: "a", Parts: []types.ToolOutputPart{types.Text("first lookup")}}),
		types.AssistantMessage{Parts: []types.AssistantPart{types.ToolCallPart{ID: "b", Name: "t"}}},
		types.ToolResults(types.ToolResultPart{CallID: "b", Parts: []types.ToolOutputPart{types.Text("second lookup")}}),
	}
}

func compactionDeltas(deltas []types.Delta) []types.CompactionDelta {
	var out []types.CompactionDelta
	for _, d := range deltas {
		if c, ok := d.(types.CompactionDelta); ok {
			out = append(out, c)
		}
	}
	return out
}

// nestedCompactionDeltas returns compaction deltas a sub-agent forwarded.
func nestedCompactionDeltas(deltas []types.Delta) []types.CompactionDelta {
	var out []types.CompactionDelta
	for _, d := range deltas {
		for {
			ex, ok := d.(types.ToolExecDelta)
			if !ok {
				break
			}
			d = ex.Inner
			if c, ok := d.(types.CompactionDelta); ok {
				out = append(out, c)
			}
		}
	}
	return out
}

func TestCompactionRecordsWhatItKeptAndDropped(t *testing.T) {
	script := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("done")}}
	a := must.Get(New(Config{
		Provider:     script,
		SystemPrompt: "sys",
		MaxIter:      1,
		CompactCfg:   &types.CompactConfig{Strategy: types.CompactKeepRecent, KeepTurns: 1, MaxInputTokens: 1},
	}))
	stream := a.Invoke(context.Background(), twoToolTurns())
	deltas := agenttest.CollectDeltas(stream.Deltas())
	if err := stream.Wait(); err != nil {
		t.Fatal(err)
	}
	got := compactionDeltas(deltas)
	if len(got) != 1 {
		t.Fatalf("compaction deltas = %d, want 1", len(got))
	}
	d := got[0]
	rec := d.Record
	if rec.Strategy != "keep_recent" || rec.Trigger != types.CompactionTriggerInputPressure || rec.FromBranch != "main" {
		t.Fatalf("record = %+v", rec)
	}
	if rec.TokensBefore <= rec.TokensAfter || rec.TokensAfter == 0 {
		t.Fatalf("tokens %d -> %d", rec.TokensBefore, rec.TokensAfter)
	}
	if len(rec.Dropped) != 2 || len(rec.Kept) != 4 || len(rec.Summarized) != 0 {
		t.Fatalf("kept %v dropped %v summarized %v", rec.Kept, rec.Dropped, rec.Summarized)
	}
	if rec.Kept[0] != a.Tree().Root().ID {
		t.Fatal("the system prompt is not recorded as kept")
	}
	// The dropped nodes are the first tool turn on the original branch.
	main, err := a.Tree().FlattenBranchAnnotated("main")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Dropped[0] != main[2].NodeID || rec.Dropped[1] != main[3].NodeID {
		t.Fatalf("dropped %v, want the nodes of call a and its result", rec.Dropped)
	}

	// The record is on the new branch, and the provider never sees it.
	msgs, err := a.Tree().FlattenBranch(d.Branch)
	if err != nil {
		t.Fatal(err)
	}
	var recorded []types.CompactionPart
	for _, m := range msgs {
		if sm, ok := m.(types.SystemMessage); ok {
			for _, c := range sm.Parts {
				if cc, ok := c.(types.CompactionPart); ok {
					recorded = append(recorded, cc)
				}
			}
		}
	}
	if len(recorded) != 1 || recorded[0].Strategy != "keep_recent" {
		t.Fatalf("records on the branch = %+v", recorded)
	}
	last := script.Requests()[0].Messages
	if strings.Contains(types.MessagesToText(last), "first lookup") {
		t.Fatal("the dropped turn reached the provider")
	}
	if err := toolPairingError(last); err != nil {
		t.Fatal(err)
	}
}

func TestTreeSummaryCompactionIsRecorded(t *testing.T) {
	script := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.TextResponse("summary of the first lookup"),
		agenttest.TextResponse("done"),
	}}
	a := must.Get(New(Config{Provider: script, SystemPrompt: "sys", MaxIter: 1, CompactCfg: &types.CompactConfig{MaxInputTokens: 1}}))
	stream := a.Invoke(context.Background(), twoToolTurns())
	got := compactionDeltas(agenttest.CollectDeltas(stream.Deltas()))
	if err := stream.Wait(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("compaction deltas = %d, want 1", len(got))
	}
	rec := got[0].Record
	if rec.Strategy != "summarize" || len(rec.Summarized) == 0 || rec.SummaryNode == "" || len(rec.Kept) == 0 {
		t.Fatalf("record = %+v", rec)
	}
}

func TestSummaryIsChargedToTheBudget(t *testing.T) {
	main := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		withUsage(agenttest.TextResponse("done"), usage(100, 10)),
	}}
	cheap := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		withUsage(agenttest.TextResponse("the first lookup happened"), usage(70, 30)),
	}}
	budget := types.NewBudget(types.BudgetPolicy{MaxRequests: 10})
	a := must.Get(New(Config{
		Provider:     pricedScripted{main},
		SystemPrompt: "sys",
		MaxIter:      1,
		CompactCfg:   &types.CompactConfig{Strategy: types.CompactSummary, KeepTurns: 1, MaxInputTokens: 1},
		Budget:       budget,
	}, WithCompactProvider(cheapScripted{cheap})))
	stream := a.Invoke(context.Background(), twoToolTurns())
	deltas := agenttest.CollectDeltas(stream.Deltas())
	if err := stream.Wait(); err != nil {
		t.Fatal(err)
	}
	if cheap.CallCount() != 1 || main.CallCount() != 1 {
		t.Fatalf("summary calls = %d, turns = %d, want 1 and 1", cheap.CallCount(), main.CallCount())
	}
	if !isSummaryRequest(cheap.Requests()[0]) {
		t.Fatal("the compaction provider did not get a summary request")
	}
	if got := budget.Usage().Requests; got != 2 {
		t.Fatalf("charged requests = %d, want 2", got)
	}
	var cheapUsage types.TokenUsage
	for _, r := range budget.Breakdown() {
		if r.Model == "cheap-model" {
			cheapUsage = r.Usage
		}
	}
	if cheapUsage.Requests != 1 || cheapUsage.InputTokens != 70 {
		t.Fatalf("cheap-model usage = %+v, want the summary call", cheapUsage)
	}
	// The summary is a system message right after the task.
	sent := main.Requests()[0].Messages
	if !types.IsCompactionSummary(sent[2]) || !strings.Contains(types.MessagesToText(sent[2:3]), "the first lookup happened") {
		t.Fatalf("turn input:\n%s", types.MessagesToText(sent))
	}
	rec := compactionDeltas(deltas)[0].Record
	if rec.Strategy != "summary" || len(rec.Summarized) != 2 || rec.SummaryNode == "" {
		t.Fatalf("record = %+v", rec)
	}
}

func TestSummaryModelSwitchesTheActiveProvider(t *testing.T) {
	a := must.Get(New(Config{Provider: &namedProvider{id: "p"}}))
	sw := &modelSwitchingProvider{model: "big"}
	got := a.compactionProvider(activeContext{provider: sw}, &types.CompactConfig{SummaryModel: "small"})
	if types.ProviderModel(got) != "small" {
		t.Fatalf("summary model = %q, want small", types.ProviderModel(got))
	}
	if got := a.compactionProvider(activeContext{provider: sw}, &types.CompactConfig{}); got != sw {
		t.Fatal("without a summary model the active provider writes summaries")
	}
}

type modelSwitchingProvider struct {
	namedProvider
	model string
}

func (s *modelSwitchingProvider) Model() string { return s.model }
func (s *modelSwitchingProvider) WithTarget(t types.Target) (types.Provider, error) {
	m := string(t.Model)
	return &modelSwitchingProvider{namedProvider: s.namedProvider, model: m}, nil
}

func TestOrchestratorWithoutCompactionChildWithIt(t *testing.T) {
	parent := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("d1", "delegate_to_worker", map[string]any{"task": "look things up"}),
		agenttest.TextResponse("all done"),
	}}
	child := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("c1", "read", nil),
		agenttest.ToolCallResponse("c2", "read", nil),
		agenttest.ToolCallResponse("c3", "read", nil),
		agenttest.TextResponse("child done"),
	}}
	read := &agenttest.MockTool{Def: types.ToolDef{Name: "read", Description: "read"}, Result: strings.Repeat("x", 400)}
	cfg := Config{
		Name:         "orchestrator",
		Provider:     parent,
		SystemPrompt: "sys",
		SubAgents: []SubAgentDef{
			{
				Name: "worker", Description: "looks things up", Provider: child,
				Tools:   types.NewToolRegistry(read),
				Options: []Option{WithCompactConfig(&types.CompactConfig{Strategy: types.CompactKeepRecent, KeepTurns: 1, MaxInputTokens: 1})},
			},
			{Name: "inheritor", Description: "inherits"},
		},
	}
	a := must.Get(New(cfg, WithoutCompaction()))
	// The orchestrator's own history is long enough that any strategy
	// would compact it.
	input := append(twoToolTurns(), types.UserMsg(types.Text("now delegate")))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := a.Invoke(ctx, input)
	deltas := agenttest.CollectDeltas(stream.Deltas())
	if err := stream.Wait(); err != nil {
		t.Fatal(err)
	}
	if got := compactionDeltas(deltas); len(got) != 0 {
		t.Fatalf("the orchestrator compacted: %+v", got)
	}
	if a.Tree().Active() != "main" {
		t.Fatalf("orchestrator branch = %s, want main", a.Tree().Active())
	}
	nested := nestedCompactionDeltas(deltas)
	if len(nested) == 0 {
		t.Fatal("the child with compaction on did not compact")
	}
	for _, d := range nested {
		if d.Record.Strategy != "keep_recent" {
			t.Fatalf("child record = %+v", d.Record)
		}
	}

	// A child without its own policy inherits the orchestrator's.
	inherited := childConfig(t, a.cfg, SubAgentDef{Name: "inheritor"})
	if inherited.CompactCfg.Enabled() {
		t.Fatalf("inherited policy = %+v, want compaction off", inherited.CompactCfg)
	}
}

func TestHandoffGroupAcceptsDisabledCompaction(t *testing.T) {
	run := func(t *testing.T, opts ...Option) error {
		t.Helper()
		script := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("hi")}}
		a := must.Get(New(Config{
			Name: "entry", Provider: script, SystemPrompt: "sys",
			Handoffs: []HandoffDef{{Name: "specialist", Description: "handles the hard part"}},
		}, opts...))
		stream := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("hello"))})
		agenttest.CollectDeltas(stream.Deltas())
		return stream.Wait()
	}
	t.Run("none strategy", func(t *testing.T) {
		if err := run(t, WithCompactConfig(&types.CompactConfig{Strategy: types.CompactNone})); err != nil {
			t.Fatalf("a disabled policy was rejected: %v", err)
		}
	})
	t.Run("orchestrator turns it off", func(t *testing.T) {
		if err := run(t, WithCompactConfig(&types.CompactConfig{Strategy: types.CompactKeepRecent}), WithoutCompaction()); err != nil {
			t.Fatalf("WithoutCompaction was rejected: %v", err)
		}
	})
	t.Run("active strategy", func(t *testing.T) {
		for _, s := range []types.CompactStrategy{"", types.CompactKeepRecent, types.CompactSummary, types.CompactClearToolResults} {
			err := run(t, WithCompactConfig(&types.CompactConfig{Strategy: s}))
			if err == nil || !strings.Contains(err.Error(), "handoff context compaction") {
				t.Fatalf("strategy %q: err = %v, want the handoff rejection", s, err)
			}
		}
	})
}

func TestWithPresetSetsCompaction(t *testing.T) {
	p := fakePreset{d: types.PresetDefaults{Compaction: &types.CompactConfig{Strategy: types.CompactChain,
		Chain: []types.CompactConfig{{Strategy: types.CompactClearToolResults}, {Strategy: types.CompactSummary}}}}}
	a := must.Get(New(Config{}, WithPreset(p)))
	if a.cfg.CompactCfg == nil || a.cfg.CompactCfg.Strategy != types.CompactChain || len(a.cfg.CompactCfg.Chain) != 2 {
		t.Fatalf("CompactCfg = %+v", a.cfg.CompactCfg)
	}
	// The agent's own policy wins.
	b := must.Get(New(Config{}, WithoutCompaction(), WithPreset(p)))
	if b.cfg.CompactCfg.Enabled() {
		t.Fatal("the preset replaced the agent's policy")
	}
	// The preset's config is copied, not shared.
	a.cfg.CompactCfg.Chain[0].Strategy = types.CompactNone
	if p.d.Compaction.Chain[0].Strategy != types.CompactClearToolResults {
		t.Fatal("the agent shares the preset's config")
	}
	if !slices.Equal([]string{"chain(clear_tool_results,summary)"}, []string{p.d.Compaction.ToStrategy().Name()}) {
		t.Fatal("unexpected strategy name")
	}
}

type fakePreset struct{ d types.PresetDefaults }

func (fakePreset) Provider() types.Provider         { return &namedProvider{id: "preset"} }
func (f fakePreset) Defaults() types.PresetDefaults { return f.d }

func TestBeforeCompactionHookSkipsStrategies(t *testing.T) {
	for _, s := range []types.CompactStrategy{types.CompactKeepRecent, types.CompactSummary, types.CompactChain} {
		t.Run(string(s), func(t *testing.T) {
			script := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("done")}}
			var before, after int
			cfg := &types.CompactConfig{Strategy: s, KeepTurns: 1, MaxInputTokens: 1,
				Chain: []types.CompactConfig{{Strategy: types.CompactKeepRecent, KeepTurns: 1}}}
			if s != types.CompactChain {
				cfg.Chain = nil
			}
			a := must.Get(New(Config{Provider: script, SystemPrompt: "sys", MaxIter: 1, CompactCfg: cfg},
				WithHooks(Hooks{
					BeforeCompaction: func(_ context.Context, ev *CompactionEvent) error { before++; ev.Skip = true; return nil },
					AfterCompaction:  func(context.Context, *CompactionEvent) error { after++; return nil },
				})))
			stream := a.Invoke(context.Background(), twoToolTurns())
			deltas := agenttest.CollectDeltas(stream.Deltas())
			if err := stream.Wait(); err != nil {
				t.Fatal(err)
			}
			if before == 0 {
				t.Fatal("BeforeCompaction was not called")
			}
			if got := compactionDeltas(deltas); len(got) != 0 || a.Tree().Active() != "main" || script.CallCount() != 1 {
				t.Fatalf("compactions = %d, active = %s, calls = %d: the skip was not honored", len(got), a.Tree().Active(), script.CallCount())
			}
			if after != 0 {
				t.Fatalf("AfterCompaction ran %d times for a skipped compaction", after)
			}
		})
	}
}
