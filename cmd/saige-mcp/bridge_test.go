package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	agenttypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/tools/research"
)

// storeTool stands in for a mutating tool such as store_knowledge.
func storeTool(runs *atomic.Int64) agenttypes.Tool {
	inner := &agenttypes.ToolFunc{
		Def: agenttypes.ToolDef{Name: "store", Description: "store a fact", Capability: agenttypes.ToolCapabilityWrite,
			Parameters: agenttypes.ParameterSchema{Type: "object"}},
		Fn: func(context.Context, map[string]any) (string, error) {
			runs.Add(1)
			return "stored", nil
		},
	}
	return agenttypes.WithMarkers(inner, agenttypes.Marker{
		Kind: "human_approval", Message: "Mutating operation requires approval: store",
		Meta: map[string]any{"mutating": true},
	})
}

type richTool struct{}

func (richTool) Definition() agenttypes.ToolDef {
	return agenttypes.ToolDef{Name: "rich", Parameters: agenttypes.ParameterSchema{Type: "object"}}
}
func (richTool) Execute(context.Context, map[string]any) (string, error) { return "text", nil }
func (richTool) ExecuteRich(context.Context, map[string]any) (agenttypes.ToolResult, error) {
	return agenttypes.ToolResult{Text: "chart", Blocks: []agenttypes.ToolResultBlock{
		{Kind: agenttypes.ToolResultBlockImage, MediaType: "image/png", Data: []byte{1}},
		{Kind: agenttypes.ToolResultBlockJSON, JSON: json.RawMessage(`{"points":3}`)},
	}}, nil
}

// session connects an in-memory client to a bridged server. elicit nil means
// the client does not support elicitation.
func session(t *testing.T, b bridge, elicit func(*mcp.ElicitRequest) (*mcp.ElicitResult, error), tools ...agenttypes.Tool) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "saige-mcp", Version: "test"}, nil)
	for _, tool := range tools {
		b.register(server, tool)
	}
	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })

	opts := &mcp.ClientOptions{}
	if elicit != nil {
		opts.ElicitationHandler = func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) { return elicit(req) }
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, opts)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{"text": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func resultText(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func TestBridgeHonorsApprovalMarkers(t *testing.T) {
	accept := func(*mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"approve": true}}, nil
	}
	refuse := func(*mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"approve": false}}, nil
	}
	decline := func(*mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		return &mcp.ElicitResult{Action: "decline"}, nil
	}
	tests := []struct {
		name    string
		mode    approvalMode
		elicit  func(*mcp.ElicitRequest) (*mcp.ElicitResult, error)
		wantRun bool
		wantMsg string
	}{
		{"elicit approved", approvalElicit, accept, true, "stored"},
		{"elicit answered no", approvalElicit, refuse, false, "not approved"},
		{"elicit declined", approvalElicit, decline, false, "not approved (decline)"},
		{"client without elicitation", approvalElicit, nil, false, "--approval=host"},
		{"host mode trusts the client prompt", approvalHost, nil, true, "stored"},
		{"deny mode", approvalDeny, accept, false, "--approval=deny"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var runs atomic.Int64
			cs := session(t, bridge{approval: tt.mode}, tt.elicit, storeTool(&runs))
			res := call(t, cs, "store")
			if ran := runs.Load() == 1; ran != tt.wantRun {
				t.Errorf("tool ran=%v, want %v", ran, tt.wantRun)
			}
			if res.IsError == tt.wantRun {
				t.Errorf("IsError=%v", res.IsError)
			}
			if !strings.Contains(resultText(res), tt.wantMsg) {
				t.Errorf("text = %q, want it to mention %q", resultText(res), tt.wantMsg)
			}
		})
	}
}

func TestBridgeEnforcesEveryMarkerKind(t *testing.T) {
	accept := func(*mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"approve": true}}, nil
	}
	tests := []struct {
		name    string
		mode    approvalMode
		elicit  func(*mcp.ElicitRequest) (*mcp.ElicitResult, error)
		marker  agenttypes.Marker
		wantRun bool
	}{
		{"audit marker without elicitation", approvalElicit, nil, agenttypes.Marker{Kind: "audit"}, false},
		{"rate limit marker in deny mode", approvalDeny, accept, agenttypes.Marker{Kind: "rate_limit"}, false},
		{"unknown marker approved", approvalElicit, accept, agenttypes.Marker{Kind: "custom"}, true},
		{"unknown marker in host mode", approvalHost, nil, agenttypes.Marker{Kind: "custom"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var runs atomic.Int64
			tool := agenttypes.WithMarkers(&agenttypes.ToolFunc{
				Def: agenttypes.ToolDef{Name: "lookup", Capability: agenttypes.ToolCapabilityRead,
					Parameters: agenttypes.ParameterSchema{Type: "object"}},
				Fn: func(context.Context, map[string]any) (string, error) { runs.Add(1); return "ok", nil },
			}, tt.marker)
			if _, ok := approvalMarker(tool); !ok {
				t.Fatal("marker not recognized")
			}
			cs := session(t, bridge{approval: tt.mode}, tt.elicit, tool)
			res := call(t, cs, "lookup")
			if ran := runs.Load() == 1; ran != tt.wantRun {
				t.Errorf("tool ran=%v (%s), want %v", ran, resultText(res), tt.wantRun)
			}
			if res.IsError == tt.wantRun {
				t.Errorf("IsError=%v", res.IsError)
			}
		})
	}
}

func TestElicitationShowsMarkerAndArguments(t *testing.T) {
	var prompt string
	var runs atomic.Int64
	cs := session(t, bridge{approval: approvalElicit}, func(req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		prompt = req.Params.Message
		return &mcp.ElicitResult{Action: "cancel"}, nil
	}, storeTool(&runs))
	call(t, cs, "store")
	if !strings.Contains(prompt, "requires approval: store") || !strings.Contains(prompt, `text="x"`) {
		t.Errorf("prompt = %q", prompt)
	}
}

func TestBridgePublishesAnnotations(t *testing.T) {
	var runs atomic.Int64
	root := t.TempDir()
	cs := session(t, bridge{approval: approvalHost}, nil,
		research.NewReadFileTool(root), storeTool(&runs), richTool{})
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*mcp.ToolAnnotations{}
	for _, tool := range res.Tools {
		got[tool.Name] = tool.Annotations
	}
	if a := got["read_file"]; a == nil || !a.ReadOnlyHint {
		t.Errorf("read_file annotations = %+v, want readOnlyHint", a)
	}
	if a := got["store"]; a == nil || a.ReadOnlyHint || a.DestructiveHint == nil || !*a.DestructiveHint {
		t.Errorf("store annotations = %+v, want destructiveHint", a)
	}
	if a := got["rich"]; a != nil {
		t.Errorf("a tool with no declared capability must publish no hints, got %+v", a)
	}
}

func TestBridgeCarriesRichResults(t *testing.T) {
	cs := session(t, bridge{approval: approvalElicit}, nil, richTool{})
	res := call(t, cs, "rich")
	if resultText(res) != "chart" {
		t.Errorf("text = %q", resultText(res))
	}
	var image bool
	for _, c := range res.Content {
		if ic, ok := c.(*mcp.ImageContent); ok && ic.MIMEType == "image/png" {
			image = true
		}
	}
	if !image {
		t.Error("image block lost")
	}
	if sc, _ := json.Marshal(res.StructuredContent); string(sc) != `{"points":3}` {
		t.Errorf("structured = %s", sc)
	}
}

func TestParseApprovalMode(t *testing.T) {
	for in, want := range map[string]approvalMode{"elicit": approvalElicit, " HOST ": approvalHost, "deny": approvalDeny} {
		if got, err := parseApprovalMode(in); err != nil || got != want {
			t.Errorf("parseApprovalMode(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := parseApprovalMode("yolo"); err == nil {
		t.Error("unknown mode accepted")
	}
}
