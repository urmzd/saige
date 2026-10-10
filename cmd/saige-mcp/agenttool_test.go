package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	agenttypes "github.com/urmzd/saige/agent/types"
)

// agentSession connects an in-memory client to a server publishing at.
func agentSession(t *testing.T, b bridge, at agentTool, elicit func(*mcp.ElicitRequest) (*mcp.ElicitResult, error)) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	server := newServer("test")
	b.registerAgent(server, at)
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
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, opts).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// scriptedAgent is an agent tool whose agent replays responses and may call
// tools.
func scriptedAgent(p agenttypes.Provider, tools ...agenttypes.Tool) agentTool {
	reg := agenttypes.NewToolRegistry(tools...)
	return agentTool{
		name: defaultAgentTool, description: "test agent", gated: anyGated(reg),
		newAgent: func() (*agentsdk.Agent, error) {
			cfg := agentsdk.Config{Name: "test", Provider: p, MaxIter: 5}
			if len(tools) > 0 {
				cfg.Tools = reg
			}
			return agentsdk.New(cfg, agentsdk.WithApprovalPolicy(agentsdk.ApprovalPolicy{}))
		},
	}
}

func callAgent(t *testing.T, cs *mcp.ClientSession, task string) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: defaultAgentTool, Arguments: map[string]any{"task": task}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestAgentToolReturnsTheFinalAnswer(t *testing.T) {
	p := &agenttest.ScriptedProvider{Responses: [][]agenttypes.Delta{agenttest.TextResponse("Paris")}}
	cs := agentSession(t, bridge{approval: approvalElicit}, scriptedAgent(p), nil)

	list, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 1 {
		t.Fatalf("tools = %+v", list.Tools)
	}
	tool := list.Tools[0]
	schema, _ := json.Marshal(tool.InputSchema)
	if !strings.Contains(string(schema), `"task"`) || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
		t.Fatalf("tool = %s, annotations %+v", schema, tool.Annotations)
	}

	res := callAgent(t, cs, "Capital of France?")
	if res.IsError || resultText(res) != "Paris" {
		t.Fatalf("result = %q (error %v)", resultText(res), res.IsError)
	}
	calls := p.Requests()
	if len(calls) != 1 || !strings.Contains(lastRequestText(calls[0].Messages), "Capital of France?") {
		t.Fatalf("the agent did not receive the task: %+v", calls)
	}
}

func TestAgentToolRequiresATask(t *testing.T) {
	p := &agenttest.ScriptedProvider{}
	cs := agentSession(t, bridge{approval: approvalElicit}, scriptedAgent(p), nil)
	res := callAgent(t, cs, "")
	if !res.IsError || p.CallCount() != 0 {
		t.Fatalf("an empty task ran: %q", resultText(res))
	}
}

func TestAgentToolHonorsMarkersOfItsTools(t *testing.T) {
	tests := []struct {
		name      string
		mode      approvalMode
		approve   bool
		wantRun   bool
		wantAsked bool
		wantMsg   string
	}{
		{"elicit approved", approvalElicit, true, true, true, "stored"},
		{"elicit refused", approvalElicit, false, false, true, "not approved"},
		{"deny", approvalDeny, true, false, false, "--approval=deny"},
		{"host cannot cover inner calls", approvalHost, true, false, false, "--approval=elicit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var runs atomic.Int64
			var asked atomic.Bool
			p := &agenttest.ScriptedProvider{Responses: [][]agenttypes.Delta{
				agenttest.ToolCallResponse("c1", "store", map[string]any{"text": "x"}),
				agenttest.TextResponse("done"),
			}}
			at := scriptedAgent(p, storeTool(&runs))
			cs := agentSession(t, bridge{approval: tt.mode}, at, func(req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
				asked.Store(true)
				if !strings.Contains(req.Params.Message, "store") {
					t.Errorf("prompt = %q", req.Params.Message)
				}
				return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"approve": tt.approve}}, nil
			})
			res := callAgent(t, cs, "store x")
			if res.IsError || resultText(res) != "done" {
				t.Fatalf("result = %q", resultText(res))
			}
			if ran := runs.Load() == 1; ran != tt.wantRun {
				t.Fatalf("inner tool ran=%v, want %v", ran, tt.wantRun)
			}
			if asked.Load() != tt.wantAsked {
				t.Fatalf("asked=%v, want %v", asked.Load(), tt.wantAsked)
			}
			// The model sees the tool result or the refusal.
			calls := p.Requests()
			if len(calls) != 2 || !strings.Contains(toolResultText(calls[1].Messages), tt.wantMsg) {
				t.Fatalf("second request = %q, want %q", toolResultText(calls[len(calls)-1].Messages), tt.wantMsg)
			}
		})
	}
}

func TestAgentToolPublishedDestructiveWhenItsToolsNeedApproval(t *testing.T) {
	var runs atomic.Int64
	cs := agentSession(t, bridge{approval: approvalElicit}, scriptedAgent(&agenttest.ScriptedProvider{}, storeTool(&runs)), nil)
	list, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if a := list.Tools[0].Annotations; a.DestructiveHint == nil || !*a.DestructiveHint {
		t.Fatalf("annotations = %+v", a)
	}
}

func TestAgentToolReturnsAStructuredResult(t *testing.T) {
	schema := &agenttypes.ParameterSchema{Type: agenttypes.SchemaObject, Required: []string{"city"},
		Properties: map[string]agenttypes.PropertyDef{"city": {Type: agenttypes.SchemaString}}}
	var sawSchema atomic.Bool
	model := &agenttest.FunctionModel{Fn: func(_ context.Context, _ []agenttypes.Message, _ []agenttypes.ToolDef, req agenttest.Request) (agenttest.Response, error) {
		sawSchema.Store(req.Schema != nil)
		return agenttest.Response{Text: `{"city": "Paris"}`}, nil
	}}
	at := agentTool{name: defaultAgentTool, schema: schema, newAgent: func() (*agentsdk.Agent, error) {
		return agentsdk.New(agentsdk.Config{Provider: model}, agentsdk.WithResponseSchema(schema))
	}}
	cs := agentSession(t, bridge{approval: approvalElicit}, at, nil)

	list, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if out, _ := json.Marshal(list.Tools[0].OutputSchema); !strings.Contains(string(out), `"city"`) {
		t.Fatalf("output schema = %s", out)
	}
	res := callAgent(t, cs, "Where?")
	if res.IsError {
		t.Fatalf("error: %s", resultText(res))
	}
	if sc, _ := json.Marshal(res.StructuredContent); string(sc) != `{"city":"Paris"}` {
		t.Fatalf("structured = %s", sc)
	}
	if !sawSchema.Load() {
		t.Fatal("the model was not sent the schema")
	}
}

func TestStructuredResultRejectsNonObjects(t *testing.T) {
	for _, answer := range []string{"no json here", `[1, 2]`} {
		res, _ := structuredResult("a", answer)
		if !res.IsError {
			t.Errorf("%q accepted", answer)
		}
	}
}

func TestLoadSchema(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	s, err := loadSchema(write("ok.json", `{"type": "object", "required": ["city"], "properties": {"city": {"type": "string"}}}`))
	if err != nil || s.Properties["city"].Type != "string" {
		t.Fatalf("schema = %+v, err %v", s, err)
	}
	if _, err := loadSchema(write("array.json", `{"type": "array"}`)); err == nil {
		t.Error("a non-object schema was accepted")
	}
	if _, err := loadSchema(write("typo.json", `{"type": "object", "propertys": {}}`)); err == nil {
		t.Error("an unknown key was accepted")
	}
}

func lastRequestText(msgs []agenttypes.Message) string {
	var sb strings.Builder
	for _, m := range msgs {
		if um, ok := m.(agenttypes.UserMessage); ok {
			for _, c := range um.Parts {
				if tc, ok := c.(agenttypes.TextPart); ok {
					sb.WriteString(tc.Text)
				}
			}
		}
	}
	return sb.String()
}

func toolResultText(msgs []agenttypes.Message) string {
	var sb strings.Builder
	add := func(c any) {
		if r, ok := c.(agenttypes.ToolResultPart); ok {
			sb.WriteString(r.Text())
		}
	}
	for _, m := range msgs {
		switch v := m.(type) {
		case agenttypes.SystemMessage:
			for _, c := range v.Parts {
				add(c)
			}
		case agenttypes.UserMessage:
			for _, c := range v.Parts {
				add(c)
			}
		}
	}
	return sb.String()
}
