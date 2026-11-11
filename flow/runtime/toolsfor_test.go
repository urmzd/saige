package runtime_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	saigetypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/sandbox/controlplane"
	"github.com/urmzd/saige/sandbox/metrics"
	"github.com/urmzd/saige/sandbox/node/inproc"
	"github.com/urmzd/saige/sandbox/tool"
	"github.com/urmzd/saige/sandbox/workspace"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/runtime"
	"github.com/urmzd/saige/flow/turn/native"
)

// spyModel answers every call with text and records the tools it was shown.
type spyModel struct {
	mu    sync.Mutex
	shown [][]saigetypes.ToolDef
}

func (m *spyModel) Stream(_ context.Context, req saigetypes.Request) (<-chan saigetypes.Delta, error) {
	m.mu.Lock()
	m.shown = append(m.shown, req.Tools)
	m.mu.Unlock()
	deltas := agenttest.TextResponse("ok")
	ch := make(chan saigetypes.Delta, len(deltas))
	for _, d := range deltas {
		ch <- d
	}
	close(ch)
	return ch, nil
}

// actorGroups expands "mcp:issues" for whoever the context acts for, and
// records who that was.
type actorGroups struct {
	mu   sync.Mutex
	seen []event.Actor
	tool *agenttest.MockTool
}

func (g *actorGroups) Expand(ctx context.Context, _, grant string) ([]saigetypes.Tool, bool, error) {
	if grant != native.MCPGroup("issues") {
		return nil, false, nil
	}
	who, _ := event.ActorFrom(ctx)
	g.mu.Lock()
	g.seen = append(g.seen, who)
	g.mu.Unlock()
	if who.ID == "" {
		return nil, true, errors.New("issues needs a person")
	}
	return []saigetypes.Tool{g.tool}, true, nil
}

// ToolsFor is how a host shows an agent's tools before running it, so it
// must be what a turn of the same request is offered: the catalog's tools,
// the groups expanded for the same actor, and a delegation tool per spawn
// grant, with the same grants left out. It calls none of them.
func TestToolsForIsWhatATurnOfTheRequestIsOffered(t *testing.T) {
	ws, err := workspace.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tools := tool.NewRegistry()
	plane := controlplane.NewMemory(inproc.NewFactory(tools, ws), metrics.Nop())
	history := newSpecs()
	history.revise("helper", "Help.")
	history.revise("triage", "Triage.")
	rev := history.edit("triage", func(s *native.Spec) {
		s.Tools = []string{"lookup", "jira_transition", native.MCPGroup("issues")}
		s.Access.Spawn = []string{"helper"}
	})

	lookup := &agenttest.MockTool{Def: saigetypes.ToolDef{Name: "lookup", Description: "Look it up", Parameters: saigetypes.ParameterSchema{Type: "object"}}}
	groups := &actorGroups{tool: &agenttest.MockTool{Def: saigetypes.ToolDef{Name: "issues__search", Parameters: saigetypes.ParameterSchema{Type: "object"}}}}
	model := &spyModel{}
	s, err := runtime.NewSandbox(plane, tools, history,
		runtime.WithCatalog(func(runtime.Request, func(event.Action) error) (native.Tools, error) {
			return native.StaticTools(lookup), nil
		}),
		runtime.WithToolGroupSource(groups),
		runtime.WithAgentOptions(native.WithProvider(native.ProviderOffline, func(native.Spec) (saigetypes.Provider, error) {
			return model, nil
		})))
	if err != nil {
		t.Fatal(err)
	}
	ana := event.Actor{ID: "U_ana", User: "u-ana"}
	req := withPin(runtime.Request{RunID: "run-1", Tenant: "acme", Attempt: 1, Text: "triage"}, rev)

	if _, err := s.Execute(event.WithActor(context.Background(), ana), req,
		func(context.Context, event.Body) error { return nil }); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(model.shown) != 1 {
		t.Fatalf("the model was asked %d times, want once", len(model.shown))
	}

	got, err := s.ToolsFor(event.WithActor(context.Background(), ana), req)
	if err != nil {
		t.Fatalf("ToolsFor: %v", err)
	}
	if !reflect.DeepEqual(got.Tools, model.shown[0]) {
		t.Errorf("ToolsFor reports\n%+v\nthe turn's model was shown\n%+v", got.Tools, model.shown[0])
	}
	var names []string
	for _, d := range got.Tools {
		names = append(names, d.Name)
	}
	if want := []string{"issues__search", "lookup", runtime.DelegateToolName("helper")}; !reflect.DeepEqual(names, want) {
		t.Errorf("tools %v, want %v", names, want)
	}
	if len(got.Unavailable) != 1 || got.Unavailable[0].Name != "jira_transition" {
		t.Errorf("unavailable %+v, want jira_transition", got.Unavailable)
	}
	if len(lookup.Calls) != 0 || len(groups.tool.Calls) != 0 || len(model.shown) != 1 {
		t.Errorf("listing called a tool or the model: lookup %d, search %d, model %d", len(lookup.Calls), len(groups.tool.Calls), len(model.shown))
	}

	// The groups are expanded for whoever the context acts for, and a
	// listing for nobody is told what a turn for nobody would be.
	nobody, err := s.ToolsFor(context.Background(), req)
	if err != nil {
		t.Fatalf("ToolsFor nobody: %v", err)
	}
	if len(nobody.Unavailable) != 2 || nobody.Unavailable[1].Name != native.MCPGroup("issues") {
		t.Errorf("for nobody, unavailable %+v, want jira_transition and mcp:issues", nobody.Unavailable)
	}
	if want := []event.Actor{ana, ana, {}}; !reflect.DeepEqual(groups.seen, want) {
		t.Errorf("the group source was asked for %+v, want %+v", groups.seen, want)
	}
}
