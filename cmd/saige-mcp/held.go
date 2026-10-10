package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	agentsdk "github.com/urmzd/saige/agent"
	agenttypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/cmd/internal/agenthost"
	"github.com/urmzd/saige/cmd/internal/approvals"
)

// resumeSuffix names the resume tool after the agent tool.
const resumeSuffix = "_resume"

// approvalsCommand is the CLI a person decides held approvals with.
const approvalsCommand = "saige approvals"

// agentRun is one agent call in progress. Its collector sends the run's
// markers and its end through events, so a call can stop at a marker and a
// later call can pick the run up again.
type agentRun struct {
	sess   *agenthost.Session[*agentRun]
	events chan runEvent
	quit   chan struct{}
	once   sync.Once
	cancel context.CancelFunc
}

// runEvent is a marker to decide, or the run's end when marker is nil.
type runEvent struct {
	marker *agenttypes.MarkerDelta
	text   string
	err    error
}

func newAgentRun(cancel context.CancelFunc) *agentRun {
	return &agentRun{events: make(chan runEvent), quit: make(chan struct{}), cancel: cancel}
}

func (r *agentRun) stream() *agentsdk.EventStream { return r.sess.Stream() }

// stop ends the run and its collector; the session calls it on close.
func (r *agentRun) stop() {
	r.once.Do(func() {
		close(r.quit)
		r.cancel()
	})
}

// collect drains the run and reports its markers and its end.
func (r *agentRun) collect(stream *agentsdk.EventStream) {
	transcript, err := agentsdk.Collect(stream, func(d agenttypes.Delta) {
		m, ok := d.(agenttypes.MarkerDelta)
		if !ok {
			return
		}
		select {
		case r.events <- runEvent{marker: &m}:
		case <-r.quit:
		}
	})
	select {
	case r.events <- runEvent{text: transcript.Text, err: err}:
	case <-r.quit:
	}
}

// heldApprovals keeps the runs whose approval waits for a person, for MCP
// clients that cannot ask their user (no elicitation). Each held call is
// recorded in the approvals store under a token; the person decides with
// saige approvals, and the model resumes the run with the resume tool.
type heldApprovals struct {
	ctx  context.Context
	runs *agenthost.Manager[*agentRun]
	dir  string
	// timeout ends a held run nobody decided; wait is how long one resume
	// call waits for a decision before it reports the call still pending.
	timeout, wait time.Duration
	// poll is the interval at which a resume call looks for a decision.
	poll time.Duration

	mu      sync.Mutex
	store   *approvals.Store
	byToken map[string]*heldCall
}

// heldCall is a run stopped at a marker.
type heldCall struct {
	run     *agentRun
	marker  agenttypes.MarkerDelta
	pending approvals.Pending
	timer   *time.Timer
	// claimed is set while a resume call waits for this decision.
	claimed bool
}

func newHeldApprovals(ctx context.Context, dir string, timeout, wait time.Duration) *heldApprovals {
	return &heldApprovals{
		ctx:     ctx,
		runs:    agenthost.NewManager[*agentRun](agenthost.Options{Max: 256, Prefix: "run_"}),
		dir:     dir,
		timeout: timeout,
		wait:    wait,
		poll:    250 * time.Millisecond,
		byToken: map[string]*heldCall{},
	}
}

// openStore opens the store on first use, so a server that never holds an
// approval writes nothing.
func (h *heldApprovals) openStore() (*approvals.Store, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.store == nil {
		s, err := approvals.Open(h.dir)
		if err != nil {
			return nil, err
		}
		h.store = s
	}
	return h.store, nil
}

// hold records the marker under a new token and returns the result that
// tells the model to have the user decide.
func (h *heldApprovals) hold(at agentTool, run *agentRun, m agenttypes.MarkerDelta) (*mcp.CallToolResult, error) {
	store, err := h.openStore()
	if err != nil {
		return nil, err
	}
	ag := run.sess.Agent()
	agentName := at.name
	if ag.Pin.Name != "" {
		agentName = ag.Pin.Name + "@" + ag.Pin.Version
	}
	now := time.Now()
	p := approvals.Pending{
		Token: approvals.NewToken(), Agent: agentName, Tool: m.ToolName, Arguments: m.Arguments,
		Message: preferredMarker(m.Markers).Message, MaxGrant: ag.MaxGrant,
		CreatedAt: now.UTC(), ExpiresAt: now.Add(h.timeout).UTC(),
	}
	if err := store.Put(p); err != nil {
		return nil, err
	}
	hc := &heldCall{run: run, marker: m, pending: p}
	h.mu.Lock()
	h.byToken[p.Token] = hc
	hc.timer = time.AfterFunc(h.timeout, func() { h.expire(p.Token, false) })
	h.mu.Unlock()
	return approvalRequired(at, p, false), nil
}

// expire ends a held run nobody decided in time. A resume call that is
// waiting keeps it until it gives up, unless force is set.
func (h *heldApprovals) expire(token string, force bool) {
	h.mu.Lock()
	hc := h.byToken[token]
	if hc == nil || hc.claimed && !force {
		h.mu.Unlock()
		return
	}
	delete(h.byToken, token)
	store := h.store
	h.mu.Unlock()
	hc.timer.Stop()
	h.runs.Remove(hc.run.sess.ID)
	if store != nil {
		_ = store.Remove(token)
	}
}

// claim takes the held call for one resume.
func (h *heldApprovals) claim(token string) (*heldCall, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	hc := h.byToken[token]
	switch {
	case hc == nil:
		return nil, "no held approval has token " + token + ": it was already resumed, it expired, or another saige-mcp process holds it"
	case hc.claimed:
		return nil, "approval " + token + " is already being resumed"
	}
	hc.claimed = true
	return hc, ""
}

// unclaim returns a held call that is still undecided, and ends it when it
// expired while the resume waited.
func (h *heldApprovals) unclaim(token string, hc *heldCall) {
	h.mu.Lock()
	hc.claimed = false
	h.mu.Unlock()
	if time.Now().After(hc.pending.ExpiresAt) {
		h.expire(token, true)
	}
}

// release drops the held call once it is decided.
func (h *heldApprovals) release(token string, hc *heldCall) {
	h.mu.Lock()
	delete(h.byToken, token)
	store := h.store
	h.mu.Unlock()
	hc.timer.Stop()
	if store != nil {
		_ = store.Remove(token)
	}
}

// close ends every held run and removes its record.
func (h *heldApprovals) close() {
	h.mu.Lock()
	tokens := make([]string, 0, len(h.byToken))
	for t := range h.byToken {
		tokens = append(tokens, t)
	}
	h.mu.Unlock()
	for _, t := range tokens {
		h.expire(t, true)
	}
	h.runs.CloseAll()
}

// registerResume publishes the tool that continues a held run.
func (b bridge) registerResume(server *mcp.Server, at agentTool) {
	t := true
	tool := &mcp.Tool{
		Name: at.name + resumeSuffix,
		Description: "Continue a " + at.name + " call that paused because one of its actions needs a person's approval. " +
			"Call it only after the user decided with `" + approvalsCommand + " approve TOKEN` or `" + approvalsCommand + " deny TOKEN`.",
		InputSchema: parameterSchemaToJSON(agenttypes.ParameterSchema{
			Type:     agenttypes.SchemaObject,
			Required: []string{"token"},
			Properties: map[string]agenttypes.PropertyDef{
				"token": {Type: agenttypes.SchemaString, Description: "The token from the approval-required result"},
			},
		}),
		Annotations: &mcp.ToolAnnotations{OpenWorldHint: &t, DestructiveHint: &t},
	}
	server.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Token string `json:"token"`
		}
		if req.Params.Arguments != nil {
			if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
				return errorResult("invalid arguments: " + err.Error()), nil
			}
		}
		return b.resume(ctx, req.Session, at, strings.TrimSpace(in.Token)), nil
	})
}

// resume waits briefly for the person's decision on token, delivers it to
// the held run, and drives the run to its end or its next approval.
func (b bridge) resume(ctx context.Context, session *mcp.ServerSession, at agentTool, token string) *mcp.CallToolResult {
	h := b.held
	if err := approvals.CheckToken(token); err != nil {
		return errorResult(err.Error())
	}
	hc, refusal := h.claim(token)
	if hc == nil {
		return errorResult(refusal)
	}
	store, err := h.openStore()
	if err != nil {
		h.unclaim(token, hc)
		return errorResult(err.Error())
	}
	deadline := time.Now().Add(h.wait)
	var e approvals.Entry
	for {
		if e, err = store.Get(token); err != nil {
			h.unclaim(token, hc)
			return errorResult(err.Error())
		}
		if e.Decision != nil {
			break
		}
		if !time.Now().Before(deadline) {
			h.unclaim(token, hc)
			return approvalRequired(at, hc.pending, true)
		}
		select {
		case <-ctx.Done():
			h.unclaim(token, hc)
			return errorResult(ctx.Err().Error())
		case <-time.After(h.poll):
		}
	}
	h.release(token, hc)
	d := e.Decision
	res := agentsdk.Resolution{Approved: d.Approved, Grant: d.Grant, Message: d.Message, Approver: d.Approver}
	if !d.Approved && res.Message == "" {
		res.Message = hc.marker.ToolName + " was not approved"
	}
	if err := hc.run.sess.Decide(hc.marker.ToolCallID, res); err != nil {
		_ = hc.run.stream().ResolveMarkerErr(hc.marker.ToolCallID, agentsdk.Resolution{
			Message: hc.marker.ToolName + " was not run: the decision was rejected: " + err.Error(),
		})
	}
	return b.drive(ctx, session, at, hc.run)
}

// approvalRequired is the result of a call that stopped for a person's
// decision. Its text tells the model to ask the user and how to continue;
// its structured content carries the same for clients that read it. An
// agent tool with an output schema gets the text only, since the content
// would not match the schema.
func approvalRequired(at agentTool, p approvals.Pending, stillPending bool) *mcp.CallToolResult {
	approve := approvalsCommand + " approve " + p.Token
	deny := approvalsCommand + " deny " + p.Token
	resumeTool := at.name + resumeSuffix
	var text strings.Builder
	if stillPending {
		fmt.Fprintf(&text, "APPROVAL STILL PENDING: no decision has been recorded for %s yet.\n", p.Token)
	} else {
		fmt.Fprintf(&text, "APPROVAL REQUIRED: the %s agent paused before running %s, which needs a person's approval.\n", p.Agent, p.Tool)
	}
	if p.Message != "" {
		fmt.Fprintf(&text, "Reason: %s\n", p.Message)
	}
	fmt.Fprintf(&text, "Arguments: %s\n\n", summarizeArgs(p.Arguments))
	fmt.Fprintf(&text, "Do not decide this yourself. Show the user the action above and ask them to run one of these commands in a terminal:\n  %s\n  %s\n", approve, deny)
	fmt.Fprintf(&text, "When they have, call %s with {\"token\": %q}. The run waits until %s.", resumeTool, p.Token, p.ExpiresAt.Format(time.RFC3339))
	res := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text.String()}}}
	if at.schema == nil {
		status := "approval_required"
		if stillPending {
			status = "approval_pending"
		}
		res.StructuredContent = map[string]any{
			"status": status, "token": p.Token, "agent": p.Agent, "tool": p.Tool,
			"arguments": p.Arguments, "message": p.Message,
			"approve_command": approve, "deny_command": deny,
			"resume_tool": resumeTool, "expires_at": p.ExpiresAt.Format(time.RFC3339),
		}
	}
	return res
}
