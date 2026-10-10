package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/provider/preset"
	agenttypes "github.com/urmzd/saige/agent/types"
)

// defaultAgentTool is the agent tool's name unless --agent-tool sets one.
const defaultAgentTool = "saige_agent"

// agentFlags are the --agent-* flags.
type agentFlags struct {
	ref, catalog, name, description, system, schemaFile string
	maxIter                                             int
	timeout                                             time.Duration
}

// agentTool is a saige agent published as one MCP tool. Each call runs a
// fresh agent on the task, so calls share no conversation.
type agentTool struct {
	name, description string
	// schema, when set, makes the agent answer with a JSON object that
	// matches it, returned as the tool's structured content.
	schema *agenttypes.ParameterSchema
	// newAgent builds the agent for one call.
	newAgent func() *agentsdk.Agent
	// newBound, when set, replaces newAgent: it binds a definition for one
	// call, and release frees what the binding opened.
	newBound func(context.Context) (a *agentsdk.Agent, release func(), err error)
	// gated reports whether any of the agent's tools needs approval, which
	// publishes the agent tool as destructive.
	gated   bool
	timeout time.Duration
}

// newAgentTool builds the agent tool from the flags. The agent runs the
// preset (or provider/model) named by --agent and can call every tool in
// tools, under the same approval rules as a direct call.
func newAgentTool(ctx context.Context, f agentFlags, tools *agenttypes.ToolRegistry) (agentTool, error) {
	if _, clash := tools.Get(f.name); clash {
		return agentTool{}, fmt.Errorf("--agent-tool %q collides with a pack tool", f.name)
	}
	cat := catalog.Default()
	if f.catalog != "" {
		var err error
		if cat, err = catalog.LoadFile(f.catalog); err != nil {
			return agentTool{}, err
		}
	}
	bundle, err := preset.Build(ctx, cat, f.ref, nil, preset.Options{})
	if err != nil {
		return agentTool{}, fmt.Errorf("--agent %s: %w", f.ref, err)
	}
	var schema *agenttypes.ParameterSchema
	if f.schemaFile != "" {
		if schema, err = loadSchema(f.schemaFile); err != nil {
			return agentTool{}, err
		}
	}
	at := agentTool{
		name: f.name, description: f.description, schema: schema, timeout: f.timeout,
		gated: anyGated(tools),
		newAgent: func() *agentsdk.Agent {
			cfg := agentsdk.AgentConfig{Name: f.name, SystemPrompt: f.system, MaxIter: f.maxIter}
			if len(tools.Definitions()) > 0 {
				cfg.Tools = tools
			}
			opts := []agentsdk.AgentOption{agentsdk.WithPreset(bundle)}
			if schema != nil {
				opts = append(opts, agentsdk.WithResponseSchema(schema))
			}
			return agentsdk.NewAgent(cfg, opts...)
		},
	}
	if at.description == "" {
		at.description = "Hand a task to a saige agent (" + f.ref + "), which works on it with its own tools and returns the final answer."
	}
	return at, nil
}

// loadSchema reads a response schema: a JSON object schema with type,
// properties and required.
func loadSchema(path string) (*agenttypes.ParameterSchema, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the path is the operator's own flag
	if err != nil {
		return nil, fmt.Errorf("--agent-schema: %w", err)
	}
	var s agenttypes.ParameterSchema
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("--agent-schema %s: %w", path, err)
	}
	if s.Type != agenttypes.SchemaObject {
		return nil, fmt.Errorf("--agent-schema %s: the schema must describe an object, got type %q", path, s.Type)
	}
	return &s, nil
}

func anyGated(r *agenttypes.ToolRegistry) bool {
	for _, d := range r.Definitions() {
		t, _ := r.Get(d.Name)
		if _, ok := approvalMarker(t); ok {
			return true
		}
	}
	return false
}

// registerAgent publishes the agent tool. Its input is {"task": "..."}; its
// output is the agent's final answer, or with a schema the structured result
// as both JSON text and structured content.
func (b bridge) registerAgent(server *mcp.Server, at agentTool) {
	t := true
	tool := &mcp.Tool{
		Name:        at.name,
		Description: at.description,
		InputSchema: parameterSchemaToJSON(agenttypes.ParameterSchema{
			Type:     agenttypes.SchemaObject,
			Required: []string{"task"},
			Properties: map[string]agenttypes.PropertyDef{
				"task": {Type: agenttypes.SchemaString, Description: "The task for the agent, with everything it needs to know"},
			},
		}),
		Annotations: &mcp.ToolAnnotations{OpenWorldHint: &t},
	}
	if at.gated {
		tool.Annotations.DestructiveHint = &t
	} else {
		f := false
		tool.Annotations.DestructiveHint = &f
	}
	if at.schema != nil {
		tool.OutputSchema = parameterSchemaToJSON(*at.schema)
	}
	server.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Task string `json:"task"`
		}
		if req.Params.Arguments != nil {
			if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
				return errorResult("invalid arguments: " + err.Error()), nil
			}
		}
		if in.Task == "" {
			return errorResult("task is required"), nil
		}
		if at.timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, at.timeout)
			defer cancel()
		}
		answer, err := b.runAgent(ctx, req.Session, at, in.Task)
		if err != nil {
			return errorResult(at.name + " failed: " + err.Error()), nil
		}
		if at.schema == nil {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: answer}}}, nil
		}
		return structuredResult(at.name, answer)
	})
}

// runAgent runs one agent on task and returns its final text. Every marker
// the agent raises is decided as a direct call to the tool would be, so the
// agent cannot run a tool the client could not run itself.
func (b bridge) runAgent(ctx context.Context, session *mcp.ServerSession, at agentTool, task string) (string, error) {
	var a *agentsdk.Agent
	if at.newBound != nil {
		var release func()
		var err error
		if a, release, err = at.newBound(ctx); err != nil {
			return "", err
		}
		defer release()
	} else {
		a = at.newAgent()
	}
	stream := a.Invoke(ctx, []agenttypes.Message{agenttypes.NewUserMessage(task)})
	transcript, err := agentsdk.Collect(stream, func(d agenttypes.Delta) {
		m, ok := d.(agenttypes.MarkerDelta)
		if !ok {
			return
		}
		if refusal := b.approveInner(ctx, session, m); refusal != "" {
			stream.ResolveMarkerWithMessage(m.ToolCallID, false, nil, refusal)
			return
		}
		stream.ResolveMarker(m.ToolCallID, true, nil)
	})
	if err != nil {
		return "", err
	}
	return transcript.Text, nil
}

// approveInner decides a marker raised inside the agent. elicit and deny
// behave as for a direct call. host cannot apply: the client's permission
// prompt covered the agent tool, not the calls the agent makes, so those are
// refused.
func (b bridge) approveInner(ctx context.Context, session *mcp.ServerSession, m agenttypes.MarkerDelta) string {
	if b.approval == approvalHost {
		return fmt.Sprintf("%s requires human approval, and the MCP client cannot see calls an agent makes; "+
			"run saige-mcp with --approval=elicit to approve them", m.ToolName)
	}
	marker := preferredMarker(m.Markers)
	return b.approve(ctx, session, m.ToolName, marker, m.Arguments)
}

// preferredMarker picks the marker whose message describes the approval,
// as approvalMarker does for a tool's own markers.
func preferredMarker(markers []agenttypes.Marker) agenttypes.Marker {
	for _, m := range markers {
		if mutating, _ := m.Meta["mutating"].(bool); m.Kind == kindHumanApproval || mutating {
			return m
		}
	}
	if len(markers) > 0 {
		return markers[0]
	}
	return agenttypes.Marker{Kind: kindHumanApproval}
}

// structuredResult returns an answer that must be a JSON object as
// structured content, with the JSON as its text.
func structuredResult(name, answer string) (*mcp.CallToolResult, error) {
	raw, err := agentsdk.ExtractJSON(answer)
	if err != nil {
		return errorResult(name + " did not return a structured result: " + err.Error()), nil
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil || obj == nil {
		if err == nil {
			err = errors.New("not a JSON object")
		}
		return errorResult(name + " did not return a structured result: " + err.Error()), nil
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: raw}},
		StructuredContent: obj,
	}, nil
}
