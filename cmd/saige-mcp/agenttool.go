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
	"github.com/urmzd/saige/cmd/internal/agenthost"
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
	newAgent func() (*agentsdk.Agent, error)
	// newBound, when set, replaces newAgent: it binds a definition for one
	// call, and release frees what the binding opened.
	newBound func(context.Context) (a *agentsdk.Agent, release func(), err error)
	// newSession, when set, replaces both: it binds a definition for one
	// call with its grant cap.
	newSession func(context.Context) (agenthost.Agent, error)
	// gated reports whether any of the agent's tools needs approval, which
	// publishes the agent tool as destructive.
	gated   bool
	timeout time.Duration
}

// build makes the agent for one call. An agent built without a definition
// gets an empty approval policy, so a grant a person attaches to a held
// approval takes effect for the rest of the call.
func (at agentTool) build(ctx context.Context) (agenthost.Agent, error) {
	if at.newSession != nil {
		return at.newSession(ctx)
	}
	if at.newBound != nil {
		a, release, err := at.newBound(ctx)
		if err != nil {
			return agenthost.Agent{}, err
		}
		return agenthost.Agent{Agent: a, Release: release}, nil
	}
	a, err := at.newAgent()
	if err != nil {
		return agenthost.Agent{}, err
	}
	return agenthost.Agent{Agent: a}, nil
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
	bundle, err := preset.Build(ctx, cat, agenttypes.PresetName(f.ref), nil, preset.Options{})
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
		newAgent: func() (*agentsdk.Agent, error) {
			cfg := agentsdk.Config{Name: f.name, SystemPrompt: f.system, MaxIter: f.maxIter}
			if len(tools.Definitions()) > 0 {
				cfg.Tools = tools
			}
			opts := []agentsdk.Option{agentsdk.WithPreset(bundle), agentsdk.WithApprovalPolicy(agentsdk.ApprovalPolicy{})}
			if schema != nil {
				opts = append(opts, agentsdk.WithResponseSchema(schema))
			}
			return agentsdk.New(cfg, opts...)
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
// as both JSON text and structured content. When the bridge can hold
// approvals and the agent may ask for one, it also publishes the resume
// tool that continues a held run.
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
		run, err := b.startAgent(ctx, at, in.Task)
		if err != nil {
			return errorResult(at.name + " failed: " + err.Error()), nil
		}
		return b.drive(ctx, req.Session, at, run), nil
	})
	if at.gated && b.approval == approvalElicit && b.held != nil {
		b.registerResume(server, at)
	}
}

// startAgent builds the agent for one call and starts it on task. The run
// lives on the bridge's context, bounded by the agent timeout, so a held
// approval can outlast the call that raised it.
func (b bridge) startAgent(ctx context.Context, at agentTool, task string) (*agentRun, error) {
	base := context.WithoutCancel(ctx)
	if b.held != nil {
		base = b.held.ctx
	}
	runCtx, cancel := context.WithCancel(base)
	if at.timeout > 0 {
		runCtx, cancel = context.WithTimeout(base, at.timeout)
	}
	ag, err := at.build(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	run := newAgentRun(cancel)
	release := ag.Release
	ag.Release = func() {
		run.stop()
		if release != nil {
			release()
		}
	}
	sess, err := b.runs().Create("", run, func() (agenthost.Agent, error) { return ag, nil })
	if err != nil {
		ag.Release()
		return nil, err
	}
	run.sess = sess
	stream, err := sess.Start(runCtx, agenttypes.UserMsg(agenttypes.Text(task)))
	if err != nil {
		b.runs().Remove(sess.ID)
		return nil, err
	}
	go run.collect(stream)
	return run, nil
}

// runs returns the sessions of running agent calls.
func (b bridge) runs() *agenthost.Manager[*agentRun] {
	if b.held != nil {
		return b.held.runs
	}
	return directRuns
}

// directRuns holds the runs of a bridge that cannot hold approvals: each
// lasts one call.
var directRuns = func() *agenthost.Manager[*agentRun] {
	m, err := agenthost.New[*agentRun](agenthost.Config{Max: 1 << 20, Prefix: "run_"})
	if err != nil {
		panic(err) // a constant, valid configuration
	}
	return m
}()

// drive waits for the run's next event: its end, or a marker to decide.
// Every marker the agent raises is decided as a direct call to the tool
// would be, so the agent cannot run a tool the client could not run itself.
// A marker the client cannot be asked about is held, when the bridge can
// hold approvals, and the call returns an approval-required result.
func (b bridge) drive(ctx context.Context, session *mcp.ServerSession, at agentTool, run *agentRun) *mcp.CallToolResult {
	for {
		var ev runEvent
		select {
		case ev = <-run.events:
		case <-ctx.Done():
			// The client gave up on the call; nothing can resume it.
			b.runs().Remove(run.sess.ID)
			return errorResult(at.name + " failed: " + ctx.Err().Error())
		}
		if ev.marker == nil {
			b.runs().Remove(run.sess.ID)
			if ev.err != nil {
				return errorResult(at.name + " failed: " + ev.err.Error())
			}
			if at.schema == nil {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: ev.text}}}
			}
			res, _ := structuredResult(at.name, ev.text)
			return res
		}
		m := *ev.marker
		if b.approval == approvalElicit && b.held != nil && (session == nil || !supportsElicitation(session)) {
			res, err := b.held.hold(at, run, m)
			if err != nil {
				run.stream().ResolveMarkerWithMessage(m.ToolCallID, false, nil, m.ToolName+" requires human approval, and it could not be held: "+err.Error())
				continue
			}
			return res
		}
		if refusal := b.approveInner(ctx, session, m); refusal != "" {
			run.stream().ResolveMarkerWithMessage(m.ToolCallID, false, nil, refusal)
			continue
		}
		run.stream().ResolveMarker(m.ToolCallID, true, nil)
	}
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
