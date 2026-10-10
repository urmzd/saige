package agent

import (
	"context"
	"reflect"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/provider/router"
	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
)

// dialModel is a scripted provider that declares a catalog model, so dials
// compile against real capabilities.
type dialModel struct {
	*agenttest.ScriptedProvider
	caps types.ModelCapabilities
}

func (p dialModel) Name() string                           { return p.caps.Provider }
func (p dialModel) Model() string                          { return p.caps.Model }
func (p dialModel) Capabilities() types.ModelCapabilities  { return p.caps }
func (p dialModel) EffectiveOptions() types.RequestOptions { return types.RequestOptions{} }

func newDialModel(provider, model string, responses ...[]types.Delta) dialModel {
	return dialModel{ScriptedProvider: &agenttest.ScriptedProvider{Responses: responses}, caps: catalog.MustLookup(types.ProviderName(provider), model)}
}

func routed(t *testing.T, p types.Provider) types.Provider {
	t.Helper()
	r, err := router.New(router.Config{Profiles: []router.Profile{{ID: "only", Provider: p}}})
	if err != nil {
		t.Fatal(err)
	}
	return r.Session()
}

func dialConfig(d types.Dials) types.UserMessage {
	return types.UserMessage{Parts: []types.UserPart{types.ConfigPart{Dials: &d, Reason: "policy"}}}
}

func depthDial(d types.Depth) types.Dials {
	return types.Dials{Reasoning: &types.ReasoningDial{Depth: d}}
}

func scopes(layers []types.DialLayer) []string {
	var out []string
	for _, l := range layers {
		out = append(out, l.Scope)
	}
	return out
}

// routesOf returns the RouteDeltas a run emitted.
func routesOf(ds []types.Delta) []types.RouteDelta {
	var out []types.RouteDelta
	for _, d := range ds {
		if r, ok := d.(types.RouteDelta); ok {
			out = append(out, r)
		}
	}
	return out
}

func TestAgentSendsDialScopes(t *testing.T) {
	p := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("ok")}}
	focused := types.CreativityFocused
	a := NewAgent(AgentConfig{Provider: p}, WithDials(types.Dials{Creativity: &focused}))
	input := types.UserMessage{Parts: []types.UserPart{types.ConfigPart{Dials: &types.Dials{Reasoning: &types.ReasoningDial{Depth: types.DepthHigh}}}, types.TextPart{Text: "hi"}}}
	agenttest.CollectDeltas(a.Invoke(context.Background(), []types.Message{input}).Deltas())
	calls := p.Requests()
	if len(calls) != 1 || calls[0].Options == nil {
		t.Fatalf("calls: %+v", calls)
	}
	layers := calls[0].Options.DialLayers
	if !reflect.DeepEqual(scopes(layers), []string{types.DialScopeAgent, types.DialScopeTurn}) {
		t.Fatalf("scopes: %+v", layers)
	}
	if *layers[0].Dials.Creativity != focused || layers[1].Dials.Reasoning.Depth != types.DepthHigh || layers[1].Hold != nil {
		t.Fatalf("layers: %+v", layers)
	}
}

func TestSubAgentInheritsDialsUnlessOverridden(t *testing.T) {
	focused, creative := types.CreativityFocused, types.CreativityCreative
	parent := AgentConfig{Provider: &agenttest.ScriptedProvider{}, Dials: types.Dials{Creativity: &focused}, DialPolicy: &types.StrictDials}
	child := childConfig(t, parent, SubAgentDef{Name: "worker"})
	if child.Dials.Creativity == nil || *child.Dials.Creativity != focused || child.DialPolicy == nil || !child.DialPolicy.Strict {
		t.Fatalf("inherited: %+v %+v", child.Dials, child.DialPolicy)
	}
	child = childConfig(t, parent, SubAgentDef{Name: "worker", Options: []AgentOption{WithDials(types.Dials{Creativity: &creative})}})
	if *child.Dials.Creativity != creative {
		t.Fatalf("override: %+v", child.Dials)
	}
}

func TestHandoffMemberUsesItsOwnDials(t *testing.T) {
	focused, creative := types.CreativityFocused, types.CreativityCreative
	a := NewAgent(AgentConfig{Name: "front", Provider: &agenttest.ScriptedProvider{}, Dials: types.Dials{Creativity: &focused}},
		WithHandoffs(HandoffDef{Name: "writer", Dials: &types.Dials{Creativity: &creative}}, HandoffDef{Name: "triage"}))
	for _, tc := range []struct {
		member, scope string
		want          types.Creativity
	}{{"writer", types.DialScopeMember, creative}, {"triage", types.DialScopeAgent, focused}, {"", types.DialScopeAgent, focused}} {
		ac := a.resolveActive(&resolvedConfig{activeAgent: tc.member, maxIter: 1}, nil)
		if ac.dialScope != tc.scope || *ac.dials.Creativity != tc.want {
			t.Errorf("%q: scope %s dials %+v", tc.member, ac.dialScope, ac.dials)
		}
	}
}

// signedToolTurn is an assistant turn with signed reasoning and a tool call,
// which opens a loop that must keep its reasoning.
func signedToolTurn(id string) types.AssistantMessage {
	return types.AssistantMessage{Parts: []types.AssistantPart{
		types.ThinkingPart{Text: "plan", Signature: "sig"},
		types.ToolCallPart{ID: id, Name: "lookup", Arguments: map[string]any{}},
	}}
}

// openLoopTree holds a conversation in the middle of a signed tool loop
// that started with reasoning depth low.
func openLoopTree(t *testing.T) *tree.Tree {
	t.Helper()
	tr, err := tree.New(types.SystemMsg(types.Text("sys")))
	if err != nil {
		t.Fatal(err)
	}
	low := depthDial(types.DepthLow)
	cur := tr.Root().ID
	for _, m := range []types.Message{
		types.UserMessage{Parts: []types.UserPart{types.ConfigPart{Dials: &low}, types.TextPart{Text: "go"}}},
		signedToolTurn("t1"),
		types.ToolResults(types.ToolResultPart{CallID: "t1", Parts: []types.ToolOutputPart{types.Text("found")}}),
	} {
		n, err := tr.AddChild(context.Background(), cur, m)
		if err != nil {
			t.Fatal(err)
		}
		cur = n.ID
	}
	return tr
}

// Scenario (e): a policy writes a reasoning change mid-conversation. While
// the signed tool loop is open the mode change is deferred; at the next
// user turn it applies, with a new effective hash and the cache reset the
// row declares.
func TestReasoningChangeDeferredInSignedLoop(t *testing.T) {
	model := newDialModel("anthropic", "claude-haiku-5-5", agenttest.TextResponse("done"), agenttest.TextResponse("again"))
	tr := openLoopTree(t)
	a := NewAgent(AgentConfig{Provider: routed(t, model), Tree: tr})

	adaptive := types.Dials{Reasoning: &types.ReasoningDial{Mode: types.ReasoningAdaptive}}
	first := routesOf(agenttest.CollectDeltas(a.Invoke(context.Background(), []types.Message{dialConfig(adaptive)}).Deltas()))
	if len(first) != 1 || first[0].Dials == nil {
		t.Fatalf("first routes: %+v", first)
	}
	d, _ := first[0].Dials.Decision(types.DialReasoning)
	if d.Action != types.DialDeferred || d.Requested != "adaptive" || d.Sent != "effort=low" {
		t.Fatalf("inside the loop: %+v", d)
	}
	if hold := model.Requests()[0].Options.DialLayers[0].Hold; hold == nil || hold.Depth != types.DepthLow {
		t.Fatalf("the turn layer does not hold the loop's reasoning: %+v", model.Requests()[0].Options.DialLayers)
	}

	second := routesOf(agenttest.CollectDeltas(a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("next"))}).Deltas()))
	d, _ = second[0].Dials.Decision(types.DialReasoning)
	if d.Action != types.DialApplied || !d.CacheResetExpected {
		t.Fatalf("at the next user turn: %+v", d)
	}
	if first[0].Dials.EffectiveHash == second[0].Dials.EffectiveHash {
		t.Fatal("the effective hash did not change")
	}
}

// An OutcomePolicy can raise reasoning depth on the same model. The switch
// is recorded as ConfigPart and reaches the next call; the same switch
// again changes nothing and is ignored.
func TestOutcomePolicyRaisesDepthOnSameModel(t *testing.T) {
	high := depthDial(types.DepthHigh)
	policy := types.OutcomePolicyFunc(func(context.Context, types.Outcome) (*types.Switch, error) {
		return &types.Switch{Dials: &high, Reason: "escalate"}, nil
	})
	p := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("ok")}}
	a := NewAgent(AgentConfig{Provider: p, SystemPrompt: "s"}, WithOutcomePolicy(policy))
	tr, branch := a.Tree(), a.Tree().Active()
	var routes []types.RouteDelta
	emit := func(d types.Delta) {
		if r, ok := d.(types.RouteDelta); ok {
			routes = append(routes, r)
		}
	}
	o := types.Outcome{Kind: types.OutcomeEvalScore, Model: "m"}
	sw, err := a.observeOutcome(context.Background(), emit, tr, branch, p, o)
	if err != nil || sw == nil || sw.Model != "" || sw.Dials == nil || sw.Dials.Reasoning.Depth != types.DepthHigh {
		t.Fatalf("switch: %+v %v", sw, err)
	}
	if len(routes) != 1 || routes[0].Reason != "escalate" {
		t.Fatalf("routes: %+v", routes)
	}
	if again, err := a.observeOutcome(context.Background(), emit, tr, branch, p, o); err != nil || again != nil {
		t.Fatalf("an unchanged switch must be ignored: %+v %v", again, err)
	}
	agenttest.CollectDeltas(a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("hi"))}).Deltas())
	layers := p.Requests()[0].Options.DialLayers
	if len(layers) != 1 || layers[0].Scope != types.DialScopeTurn || layers[0].Dials.Reasoning.Depth != types.DepthHigh {
		t.Fatalf("the switch did not reach the call: %+v", layers)
	}
}

// A durable replay returns the recorded turn, so its dial decisions are
// the ones the live call made.
func TestDurableReplayKeepsDialDecisions(t *testing.T) {
	focused := types.CreativityFocused
	runner := newRecordingRunner()
	live := newDialModel("openai", "gpt-6-luna", agenttest.TextResponse("live"))
	a := NewAgent(AgentConfig{Provider: routed(t, live), SystemPrompt: "s"}, WithDials(types.Dials{Creativity: &focused}))
	first, err := a.RunDurable(context.Background(), runner, []types.Message{types.UserMsg(types.Text("hi"))}, "")
	if err != nil {
		t.Fatal(err)
	}
	b := NewAgent(AgentConfig{Provider: panicProvider{}, SystemPrompt: "s"}, WithDials(types.Dials{Creativity: &focused}))
	replayed, err := b.RunDurable(context.Background(), runner, []types.Message{types.UserMsg(types.Text("hi"))}, "")
	if err != nil {
		t.Fatal(err)
	}
	want, got := routeOf(first), routeOf(replayed)
	if want == nil || want.Dials == nil || !reflect.DeepEqual(want, got) {
		t.Fatalf("replayed route differs:\n got %+v\nwant %+v", got, want)
	}
	if d, _ := want.Dials.Decision(types.DialCreativity); d.Action != types.DialDropped {
		t.Fatalf("decision: %+v", d)
	}
}

func routeOf(m *types.AssistantMessage) *types.RoutePart {
	if m == nil {
		return nil
	}
	for _, c := range m.Parts {
		if r, ok := c.(types.RoutePart); ok {
			return &r
		}
	}
	return nil
}

// Dials written as ConfigPart survive a save and reload of the tree, so
// a restored conversation keeps them.
func TestReloadRestoresDials(t *testing.T) {
	high := depthDial(types.DepthHigh)
	data, err := tree.MarshalMessage(dialConfig(high))
	if err != nil {
		t.Fatal(err)
	}
	msg, err := tree.UnmarshalMessage(types.RoleUser, data)
	if err != nil {
		t.Fatal(err)
	}
	rc, _ := NewAgent(AgentConfig{Provider: &agenttest.ScriptedProvider{}}).prepareMessages([]types.Message{msg})
	if rc.dials.Reasoning == nil || rc.dials.Reasoning.Depth != types.DepthHigh {
		t.Fatalf("restored dials: %+v (%s)", rc.dials, data)
	}
}

// A single adapter reports no route, so the agent reports the dial
// decisions itself: on the stream as a RouteDelta and on the turn, whether
// or not a dial was changed.
func TestSingleAdapterReportsDials(t *testing.T) {
	focused := types.CreativityFocused
	for _, tc := range []struct {
		name   string
		dials  types.Dials
		dial   types.DialName
		action types.DialAction
	}{
		{"dropped", types.Dials{Creativity: &focused}, types.DialCreativity, types.DialDropped},
		{"applied", depthDial(types.DepthHigh), types.DialReasoning, types.DialApplied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newDialModel("anthropic", "claude-haiku-5-5", agenttest.TextResponse("ok"))
			a := NewAgent(AgentConfig{Provider: p, SystemPrompt: "s"}, WithDials(tc.dials))
			stream := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("hi"))})
			routes := routesOf(agenttest.CollectDeltas(stream.Deltas()))
			if err := stream.Wait(); err != nil {
				t.Fatal(err)
			}
			if len(routes) != 1 || routes[0].Dials == nil || routes[0].Provider != "anthropic" || routes[0].Options == nil {
				t.Fatalf("routes: %+v", routes)
			}
			if d, _ := routes[0].Dials.Decision(tc.dial); d.Action != tc.action {
				t.Fatalf("decision: %+v", d)
			}
			msgs, _ := a.Tree().FlattenBranch(a.Tree().Active())
			last, _ := msgs[len(msgs)-1].(types.AssistantMessage)
			if r := routeOf(&last); r == nil || r.Dials == nil {
				t.Fatalf("no route on the turn: %+v", last)
			}
		})
	}
	// Without dials nothing is reported, as before.
	p := newDialModel("anthropic", "claude-haiku-5-5", agenttest.TextResponse("ok"))
	stream := NewAgent(AgentConfig{Provider: p}).Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("hi"))})
	if routes := routesOf(agenttest.CollectDeltas(stream.Deltas())); len(routes) != 0 {
		t.Fatalf("routes without dials: %+v", routes)
	}
}
