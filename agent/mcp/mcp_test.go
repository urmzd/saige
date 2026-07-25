package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestSpecValidationRejectsAmbiguousTransports(t *testing.T) {
	tests := []struct {
		name string
		spec ServerSpec
		ok   bool
	}{
		{"local", Local("fs", "mcp-server-filesystem", "/tmp"), true},
		{"remote", Remote("api", "https://mcp.example.com"), true},
		{"no name", ServerSpec{Command: "x"}, false},
		{"no transport", ServerSpec{Name: "x"}, false},
		{"both transports", ServerSpec{Name: "x", Command: "y", URL: "https://z"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.spec.Validate()
			if (err == nil) != tt.ok {
				t.Errorf("Validate = %v, want ok=%v", err, tt.ok)
			}
		})
	}
}

func TestTransportKindIsExplicit(t *testing.T) {
	if !Local("fs", "cmd").IsLocal() {
		t.Error("a Command spec is local")
	}
	if Remote("api", "https://x").IsLocal() {
		t.Error("a URL spec is remote: the lifetime and trust differences depend on this")
	}
}

// Two servers exposing "search" must not collide, so names are prefixed by
// default. Without this the model calls one server's tool and reaches another's.
func TestToolNamesArePrefixedByServerByDefault(t *testing.T) {
	if got := Local("github", "x").prefix(); got != "github_" {
		t.Errorf("prefix = %q, want %q", got, "github_")
	}
	if got := (ServerSpec{Name: "a", ToolPrefix: "gh:"}).prefix(); got != "gh:" {
		t.Errorf("prefix = %q, want the override", got)
	}
	if got := (ServerSpec{Name: "a", ToolPrefix: "-"}).prefix(); got != "" {
		t.Errorf("prefix = %q, want empty when explicitly disabled", got)
	}
}

func TestSchemaConversionHandlesTheShapesMCPActuallySends(t *testing.T) {
	raw := json.RawMessage(`{
		"type": "object",
		"required": ["query"],
		"properties": {
			"query":  {"type": "string", "description": "what to search for"},
			"limit":  {"type": "integer", "default": 10},
			"tags":   {"type": "array", "items": {"type": "string"}},
			"filter": {"type": "object", "properties": {"since": {"type": "string"}}},
			"mode":   {"type": "string", "enum": ["fast", "thorough"]}
		}
	}`)

	got := schemaFromMCP(raw)

	if got.Type != "object" {
		t.Errorf("type = %q, want object", got.Type)
	}
	if len(got.Required) != 1 || got.Required[0] != "query" {
		t.Errorf("required = %v, want [query]", got.Required)
	}
	if p := got.Properties["query"]; p.Type != "string" || p.Description == "" {
		t.Errorf("query property = %+v", p)
	}
	if p := got.Properties["limit"]; p.Default == nil {
		t.Error("a default must survive conversion: the model uses it")
	}
	if p := got.Properties["tags"]; p.Items == nil || p.Items.Type != "string" {
		t.Errorf("array items lost: %+v", p)
	}
	if p := got.Properties["filter"]; len(p.Properties) != 1 {
		t.Errorf("nested object properties lost: %+v", p)
	}
	if p := got.Properties["mode"]; len(p.Enum) != 2 {
		t.Errorf("enum lost: %+v", p)
	}
}

// A schema we cannot read must still yield a callable tool: one odd tool must
// not break importing a whole server.
func TestUnreadableSchemaDegradesToAnEmptyObject(t *testing.T) {
	for _, in := range []any{nil, "not a schema", json.RawMessage(`[1,2,3]`)} {
		got := schemaFromMCP(in)
		if got.Type != "object" {
			t.Errorf("schemaFromMCP(%v).Type = %q, want object", in, got.Type)
		}
	}
}

func TestTypeUnionsPickTheGeneratableMember(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","properties":{"x":{"type":["null","integer"]}}}`)
	got := schemaFromMCP(raw)
	if p := got.Properties["x"]; p.Type != "integer" {
		t.Errorf("union type resolved to %q, want integer: null is not something the model can emit", p.Type)
	}
}

// Per-server policy is the useful unit because trust is per-server: a local
// server you wrote needs no gate, a remote third party probably does.
func TestPoolGateAppliesPerServerPolicy(t *testing.T) {
	pool := NewPool()

	gatedClient := &Client{
		spec:  ServerSpec{Name: "remote", URL: "https://x", Gate: types.DenyListGate("remote_danger")},
		tools: map[string]*serverTool{},
	}
	openClient := &Client{
		spec:  ServerSpec{Name: "local", Command: "x"},
		tools: map[string]*serverTool{},
	}
	pool.byTool["remote_danger"] = gatedClient
	pool.byTool["remote_safe"] = gatedClient
	pool.byTool["local_anything"] = openClient

	gate := pool.Gate()
	ctx := context.Background()

	if got := gate.Check(ctx, types.ToolDef{Name: "remote_danger"}, nil); got.Outcome != types.GateDeny {
		t.Error("the owning server's gate must apply to its own tools")
	}
	if got := gate.Check(ctx, types.ToolDef{Name: "remote_safe"}, nil); got.Outcome != types.GateAllow {
		t.Error("a tool the server's gate permits must pass")
	}
	if got := gate.Check(ctx, types.ToolDef{Name: "local_anything"}, nil); got.Outcome != types.GateAllow {
		t.Error("a server with no gate must not have one invented for it")
	}
	if got := gate.Check(ctx, types.ToolDef{Name: "not_from_mcp"}, nil); got.Outcome != types.GateAllow {
		t.Error("a tool the pool does not own must pass through untouched")
	}
}

func TestOwnerAttributesAToolToItsServer(t *testing.T) {
	pool := NewPool()
	c := &Client{spec: ServerSpec{Name: "github", URL: "https://x"}, tools: map[string]*serverTool{}}
	pool.byTool["github_search"] = c

	if got, ok := pool.Owner("github_search"); !ok || got.Spec().Name != "github" {
		t.Error("Owner must attribute an imported tool to the server that exposed it")
	}
	if _, ok := pool.Owner("unknown"); ok {
		t.Error("an unowned tool must not report an owner")
	}
}

func TestServerToolValidationCatchesProviderSideMistakes(t *testing.T) {
	tests := []struct {
		name string
		tool types.ServerTool
		ok   bool
	}{
		{"web search", types.WebSearchTool(5), true},
		{"both domain lists", types.ServerTool{
			Kind:           types.ServerToolWebSearch,
			AllowedDomains: []string{"a.com"},
			BlockedDomains: []string{"b.com"},
		}, false},
		{"remote mcp without server", types.ServerTool{Kind: types.ServerToolRemoteMCP}, false},
		{"remote mcp without url", types.ServerTool{
			Kind: types.ServerToolRemoteMCP, MCPServer: &types.RemoteMCPServer{Name: "x"},
		}, false},
		{"remote mcp complete", types.ServerTool{
			Kind: types.ServerToolRemoteMCP, MCPServer: &types.RemoteMCPServer{Name: "x", URL: "https://y"},
		}, true},
		{"unknown kind", types.ServerTool{Kind: "teleport"}, false},
		{"negative max uses", types.ServerTool{Kind: types.ServerToolWebSearch, MaxUses: -1}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.tool.Validate(); (err == nil) != tt.ok {
				t.Errorf("Validate = %v, want ok=%v", err, tt.ok)
			}
		})
	}
}

// Requesting a server tool the model cannot run must fail at startup, not
// mid-stream on the first search.
func TestServerToolsAreValidatedAgainstDeclaredCapabilities(t *testing.T) {
	supported := types.ModelCapabilities{
		Caps:        map[types.Capability]bool{types.CapWebSearch: true},
		ServerTools: []types.ServerToolKind{types.ServerToolWebSearch},
		Provider:    "anthropic", Model: "claude-sonnet-4-5",
	}
	if err := types.ValidateServerTools(supported, []types.ServerTool{types.WebSearchTool(3)}); err != nil {
		t.Errorf("a supported server tool must validate: %v", err)
	}

	local := types.ModelCapabilities{Caps: map[types.Capability]bool{}, Provider: "ollama", Model: "qwen3"}
	err := types.ValidateServerTools(local, []types.ServerTool{types.WebSearchTool(3)})
	if err == nil {
		t.Error("requesting web search from a model that cannot run it must fail before the request")
	}
}
