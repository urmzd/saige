package runtime_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	saigetypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/sandbox/controlplane"
	"github.com/urmzd/saige/sandbox/metrics"
	"github.com/urmzd/saige/sandbox/node/inproc"
	"github.com/urmzd/saige/sandbox/tool"
	"github.com/urmzd/saige/sandbox/workspace"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connector"
	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/runtime"
	"github.com/urmzd/saige/flow/turn/native"
)

type identityActor struct{ seen string }

func (*identityActor) Name() string { return "identity" }
func (a *identityActor) Actions() []connector.Action {
	return []connector.Action{{Name: "identity_read", Schema: json.RawMessage(`{"type":"object"}`), Invoke: func(ctx context.Context, _ address.Address, _ json.RawMessage) (connector.Result, error) {
		a.seen = connector.RunID(ctx)
		return connector.Result{Summary: a.seen}, nil
	}}}
}

func TestCatalogBindsTheActualRunOverCallerContextAndSourceParameters(t *testing.T) {
	actor := &identityActor{}
	reg := connector.NewRegistry()
	if err := reg.Register(actor); err != nil {
		t.Fatal(err)
	}
	catalog, err := runtime.Connectors(reg)(runtime.Request{RunID: "actual-run", Source: "identity://acme/subject?write_run=forged-source"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tool, err := catalog.Lookup("identity_read")
	if err != nil {
		t.Fatal(err)
	}
	result, err := tool.Execute(connector.WithRunID(context.Background(), "forged-context"), map[string]any{})
	if err != nil || result != "actual-run" || actor.seen != "actual-run" {
		t.Fatalf("result %q seen %q error %v", result, actor.seen, err)
	}
}

// groupCatalog is a per-turn catalog that resolves no single tools and one
// MCP tool group, the way a host serving a tenant's MCP providers would.
type groupCatalog struct{ group native.ToolGroup }

func (groupCatalog) Lookup(name string) (saigetypes.Tool, error) {
	return nil, fmt.Errorf("%w: %q", native.ErrNoSuchTool, name)
}

func (c groupCatalog) ToolGroup(name string) (native.ToolGroup, error) {
	if name != c.group.Name {
		return native.ToolGroup{}, fmt.Errorf("%w: %q", native.ErrNoSuchToolGroup, name)
	}
	return c.group, nil
}

// echoTool is an MCP-style tool qualified by its provider.
type echoTool struct{}

func (echoTool) Definition() saigetypes.ToolDef {
	return saigetypes.ToolDef{Name: "urmzd__echo", Parameters: saigetypes.ParameterSchema{Type: "object"}}
}
func (echoTool) Execute(context.Context, map[string]any) (string, error) { return "echoed", nil }

// A spec granting an MCP provider's tool group reaches its tools through the
// sandbox, which wraps the deployment catalog for every turn.
func TestAnMCPToolGroupResolvesThroughTheSandbox(t *testing.T) {
	ws, err := workspace.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tools := tool.NewRegistry()
	plane := controlplane.NewMemory(inproc.NewFactory(tools, ws), metrics.Nop())
	history := newSpecs()
	history.revise("lead", "Lead.")
	lead := history.edit("lead", func(s *native.Spec) { s.Tools = []string{native.MCPGroup("urmzd")} })

	catalog := groupCatalog{group: native.ToolGroup{Name: native.MCPGroup("urmzd"), Tools: []saigetypes.Tool{echoTool{}}}}
	s, err := runtime.NewSandbox(plane, tools, history,
		runtime.WithCatalog(func(runtime.Request, func(event.Action) error) (native.Tools, error) { return catalog, nil }),
		runtime.WithAgentOptions(scripted(map[string][][]saigetypes.Delta{
			"lead": {
				agenttest.ToolCallResponse("call-1", "urmzd__echo", map[string]any{}),
				agenttest.TextResponse("done"),
			},
		})))
	if err != nil {
		t.Fatal(err)
	}
	out, chunks, err := runLead(t, s, lead)
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "done" {
		t.Fatalf("answer = %q", out.Text)
	}
	if done := toolResult(t, chunks); done.Result != "echoed" || done.Error != "" {
		t.Fatalf("tool result = %+v, want the group's member to run", done)
	}
}
