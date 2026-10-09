package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
)

// SubAgentMode selects how a parent runs a sub-agent.
type SubAgentMode int

const (
	// SubAgentDelegate registers delegate_to_<name>. The parent's tool call
	// waits for the child and its result is the child's answer.
	SubAgentDelegate SubAgentMode = iota
	// SubAgentSpawn registers spawn_<name>, which starts the child in the
	// background and returns a handle at once, plus the tools that manage
	// handles: await_subagent, send_subagent, cancel_subagent,
	// list_subagents, search_subagent, and read_subagent.
	SubAgentSpawn
)

// String returns the mode's name.
func (m SubAgentMode) String() string {
	switch m {
	case SubAgentDelegate:
		return "delegate"
	case SubAgentSpawn:
		return "spawn"
	default:
		return fmt.Sprintf("SubAgentMode(%d)", int(m))
	}
}

// ErrSpawnUnsupported means a spawn was requested where no background child
// can run: outside the agent loop, or under a durable step runner, which
// cannot replay a child that outlives the tool call that started it.
var ErrSpawnUnsupported = errors.New("spawned sub-agents need an inline agent loop")

// ErrUnknownHandle means no sub-agent handle has that ID in this run.
var ErrUnknownHandle = errors.New("unknown sub-agent handle")

// HandleStatus is the state of a spawned or recorded sub-agent run.
type HandleStatus string

const (
	HandleRunning   HandleStatus = "running"
	HandleCompleted HandleStatus = "completed"
	HandleFailed    HandleStatus = "failed"
	HandleCanceled  HandleStatus = "canceled"
)

// SubAgentHandle refers to one sub-agent run started by spawn_<name>, or to
// a finished delegation kept for the transcript tools. Its ID is the tool
// call ID that started it. Methods are safe for concurrent use.
type SubAgentHandle struct {
	id, name, task string
	stream         *EventStream // nil for a recorded delegation
	done           chan struct{}

	mu        sync.Mutex
	status    HandleStatus
	result    SubAgentResult
	err       error
	delivered bool // the parent has received the result
}

// ID returns the handle ID, which is the ID of the tool call that started
// the child.
func (h *SubAgentHandle) ID() string { return h.id }

// Name returns the sub-agent definition's name.
func (h *SubAgentHandle) Name() string { return h.name }

// Task returns the task the child was given.
func (h *SubAgentHandle) Task() string { return h.task }

// Status reports whether the child is running or how it ended.
func (h *SubAgentHandle) Status() HandleStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status
}

// Done is closed when the child has finished and its result is recorded.
func (h *SubAgentHandle) Done() <-chan struct{} { return h.done }

// Wait blocks until the child finishes or ctx is done. It returns the
// detached result, which holds a partial trace when err is not nil.
func (h *SubAgentHandle) Wait(ctx context.Context) (SubAgentResult, error) {
	select {
	case <-h.done:
	case <-ctx.Done():
		return SubAgentResult{}, ctx.Err()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return cloneSubAgentResult(h.result), h.err
}

// Send steers the running child: msg joins it at its next safe point, as
// SubmitSteer does for a top-level run. It returns ErrRunFinished once the
// child has passed its last safe point.
func (h *SubAgentHandle) Send(msg types.UserMessage) (SubmissionID, error) {
	if h.stream == nil {
		return "", ErrRunFinished
	}
	return h.stream.Submit(msg, SubmitSteer)
}

// Cancel stops the child. Its result records the cancellation.
func (h *SubAgentHandle) Cancel() {
	if h.stream != nil {
		h.stream.Cancel()
	}
}

// Interrupts lists the decisions the child is waiting for.
func (h *SubAgentHandle) Interrupts() []types.Interrupt {
	if h.stream == nil {
		return nil
	}
	return h.stream.PendingInterrupts()
}

// SubAgents returns the handles of the sub-agents this run spawned, and of
// the delegations it kept for the transcript tools, in start order. It is
// empty for an agent without spawn definitions.
func (s *EventStream) SubAgents() []*SubAgentHandle {
	if s.spawns == nil {
		return nil
	}
	s.spawns.mu.Lock()
	defer s.spawns.mu.Unlock()
	return append([]*SubAgentHandle(nil), s.spawns.order...)
}

// spawnRegistry tracks one run's sub-agent handles.
type spawnRegistry struct {
	mu       sync.Mutex
	handles  map[string]*SubAgentHandle
	order    []*SubAgentHandle // start order
	finished []*SubAgentHandle // completion order
	running  int
	changed  chan struct{} // closed and replaced whenever a child finishes
	wg       sync.WaitGroup
}

func newSpawnRegistry() *spawnRegistry {
	return &spawnRegistry{handles: map[string]*SubAgentHandle{}, changed: make(chan struct{})}
}

type spawnRegistryKey struct{}

func spawnsFrom(ctx context.Context) *spawnRegistry {
	r, _ := ctx.Value(spawnRegistryKey{}).(*spawnRegistry)
	return r
}

// add registers a running child.
func (r *spawnRegistry) add(id, name, task string, stream *EventStream) *SubAgentHandle {
	h := &SubAgentHandle{id: id, name: name, task: task, stream: stream, done: make(chan struct{}), status: HandleRunning}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handles[id] = h
	r.order = append(r.order, h)
	r.running++
	return h
}

// finish records a child's result and wakes anything waiting for it.
func (r *spawnRegistry) finish(h *SubAgentHandle, result SubAgentResult, err error) {
	h.mu.Lock()
	h.result, h.err, h.status = result, err, statusOf(err)
	h.mu.Unlock()
	r.mu.Lock()
	r.finished = append(r.finished, h)
	r.running--
	close(r.changed)
	r.changed = make(chan struct{})
	r.mu.Unlock()
	close(h.done)
}

func statusOf(err error) HandleStatus {
	switch {
	case err == nil:
		return HandleCompleted
	case errors.Is(err, types.ErrStreamCanceled), errors.Is(err, context.Canceled):
		return HandleCanceled
	default:
		return HandleFailed
	}
}

// recordDelegation keeps a finished delegation's result so the transcript
// tools can read it. The parent already has its result.
func (r *spawnRegistry) recordDelegation(id, name string, result SubAgentResult, err error) {
	if r == nil {
		return
	}
	h := &SubAgentHandle{id: id, name: name, task: result.Task, done: make(chan struct{}), delivered: true}
	h.result, h.err, h.status = result, err, statusOf(err)
	close(h.done)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.handles[id]; ok {
		return
	}
	r.handles[id] = h
	r.order = append(r.order, h)
}

func (r *spawnRegistry) get(id string) (*SubAgentHandle, error) {
	if r == nil {
		return nil, ErrSpawnUnsupported
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.handles[id]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownHandle, id)
	}
	return h, nil
}

// markDelivered records that the parent received h's result, so it is not
// injected again. It reports whether this call was the first to do so.
func markDelivered(h *SubAgentHandle) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	first := !h.delivered
	h.delivered = true
	return first
}

// takeFinished returns finished children whose results the parent has not
// received, in completion order, and marks them received.
func (r *spawnRegistry) takeFinished() []*SubAgentHandle {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	finished := append([]*SubAgentHandle(nil), r.finished...)
	r.finished = nil
	r.mu.Unlock()
	var out []*SubAgentHandle
	for _, h := range finished {
		if markDelivered(h) {
			out = append(out, h)
		}
	}
	return out
}

// outstanding reports whether a child is running or a finished child's
// result has not reached the parent.
func (r *spawnRegistry) outstanding() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running > 0 {
		return true
	}
	for _, h := range r.finished {
		h.mu.Lock()
		delivered := h.delivered
		h.mu.Unlock()
		if !delivered {
			return true
		}
	}
	return false
}

// waitFinished blocks until a finished child's result is waiting for the
// parent, no child is running, or ctx is done.
func (r *spawnRegistry) waitFinished(ctx context.Context) error {
	for {
		r.mu.Lock()
		ready := r.running == 0
		for _, h := range r.finished {
			h.mu.Lock()
			if !h.delivered {
				ready = true
			}
			h.mu.Unlock()
		}
		changed := r.changed
		r.mu.Unlock()
		if ready {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// shutdown cancels every running child and waits until each has finished
// and its deltas have been forwarded. The run calls it before it closes its
// stream, so no forwarded delta can arrive on a closed stream.
func (r *spawnRegistry) shutdown() {
	if r == nil {
		return
	}
	r.mu.Lock()
	for _, h := range r.order {
		h.Cancel()
	}
	r.mu.Unlock()
	r.wg.Wait()
}

// ── Budget admission ────────────────────────────────────────────────

// childAdmission is the budget reservation a spawned child takes when it
// starts. Its first provider call uses it, so a spawn the budget cannot
// admit fails at once instead of starting a child that cannot run.
type childAdmission struct {
	mu          sync.Mutex
	reservation types.BudgetReservation
	pricing     types.Pricing
	model       string
	used        bool
}

// admitChild reserves capacity for a spawned child under its handle ID.
func admitChild(child *Agent, id string) (*childAdmission, error) {
	caps, _ := types.ProviderCapabilities(child.cfg.Provider)
	r, err := child.cfg.Budget.Reserve("spawn-"+id, caps.Pricing)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", types.ErrBudgetAdmission, err)
	}
	return &childAdmission{reservation: r, pricing: caps.Pricing, model: types.ProviderModel(child.cfg.Provider)}, nil
}

// take hands the reservation to the child's first provider call.
func (c *childAdmission) take() (types.BudgetReservation, bool) {
	if c == nil {
		return types.BudgetReservation{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.used {
		return types.BudgetReservation{}, false
	}
	c.used = true
	return c.reservation, true
}

// release settles a reservation no provider call used, with no usage, so a
// child cancelled before its first call frees its capacity.
func (c *childAdmission) release(budget *types.Budget) {
	if _, unused := c.take(); unused && budget != nil {
		_ = budget.Settle(c.reservation.ID, c.model, c.pricing, types.TokenUsage{}, false)
	}
}

// ── Spawning ────────────────────────────────────────────────────────

// spawnTool is registered as spawn_<name>. The agent loop starts the child;
// Execute only reports that it was called outside the loop.
type spawnTool struct {
	*subAgentTool
}

func (t *spawnTool) Execute(context.Context, map[string]any) (string, error) {
	return "", ErrSpawnUnsupported
}

// spawnReceipt is the tool result of spawn_<name>.
type spawnReceipt struct {
	Handle string       `json:"handle"`
	Name   string       `json:"name"`
	Status HandleStatus `json:"status"`
	Note   string       `json:"note"`
}

// spawnSubAgent starts a background child for a spawn_<name> call and
// returns its handle at once. The child runs under the run's context, not
// the tool call's, so it outlives the call; the run cancels it when the run
// ends.
func (a *Agent) spawnSubAgent(ctx context.Context, stream *EventStream, tc types.ToolUseContent, t *spawnTool) toolResult {
	reg := stream.spawns
	if _, inline := a.cfg.StepRunner.(types.NoopStepRunner); reg == nil || !inline {
		return failedTool(stream, tc.ID, tc.Name, ErrSpawnUnsupported.Error())
	}
	if err := a.checkAncestor(ctx, t.name); err != nil {
		return refusedTool(stream, tc.ID, tc.Name, "refused: "+err.Error())
	}
	history, err := a.childHistory(ctx, stream, t.context, t.filter)
	if err != nil {
		return failedTool(stream, tc.ID, tc.Name, err.Error())
	}
	task, _ := tc.Arguments[argTask].(string)

	childCtx, cancel := context.WithCancelCause(stream.ctx)
	var clock *pausableDeadline
	if t.timeout > 0 {
		timeoutErr := fmt.Errorf("sub-agent %s exceeded timeout %s: %w", t.name, t.timeout, context.DeadlineExceeded)
		clock = newPausableDeadline(t.timeout, func() { cancel(timeoutErr) })
	}
	child, err := t.start(childCtx, childRun{
		task: task, history: history, id: tc.ID, nonStreaming: stream.nonStreaming,
		frame: a.childFrame(ctx, stream, tc.ID), background: true,
	})
	if err != nil {
		clock.stop()
		cancel(nil)
		return failedTool(stream, tc.ID, tc.Name, "spawn refused: "+err.Error())
	}
	h := reg.add(tc.ID, t.name, task, child)
	reg.wg.Add(1)
	go func() {
		defer reg.wg.Done()
		defer cancel(nil)
		defer clock.stop()
		var pending sync.WaitGroup
		for d := range child.Deltas() {
			if marker, ok := d.(types.MarkerDelta); ok {
				a.forwardChildMarker(childCtx, stream, tc.ID, child, marker, clock, &pending)
				continue
			}
			if citation, ok := d.(types.CitationDelta); ok {
				a.citations.Add(citation.Citation)
			}
			stream.send(types.ToolExecDelta{ToolCallID: tc.ID, Inner: d})
		}
		pending.Wait()
		result, err := child.SubAgentResult()
		if cause := context.Cause(childCtx); err != nil && clock != nil && errors.Is(cause, context.DeadlineExceeded) {
			err = cause
		}
		reg.finish(h, result, err)
	}()

	raw, _ := json.Marshal(spawnReceipt{
		Handle: tc.ID, Name: t.name, Status: HandleRunning,
		Note: "The sub-agent runs in the background. Its result arrives as a message when it finishes, or call await_subagent with this handle.",
	})
	res := toolResult{toolCallID: tc.ID, result: string(raw)}
	stream.send(types.ToolExecEndDelta{ToolCallID: tc.ID, Name: tc.Name, Result: res.result})
	return res
}

// injectSpawned appends the results of finished children that the parent
// has not received, one user message each, in completion order. It runs only
// at safe points.
func (a *Agent) injectSpawned(ctx context.Context, stream *EventStream, tr *tree.Tree, branch types.BranchID) (int, error) {
	finished := stream.spawns.takeFinished()
	for _, h := range finished {
		node, err := a.appendNode(ctx, tr, branch, spawnResultMessage(h))
		if err != nil {
			return 0, err
		}
		stream.send(types.InjectedDelta{SubmissionID: h.id, Mode: "subagent", NodeID: string(node.ID)})
	}
	return len(finished), nil
}

// spawnResultMessage wraps a finished child's result for the parent. The
// output is data from the child, not an instruction, so a closing tag inside
// it is escaped and cannot end the wrapper early.
func spawnResultMessage(h *SubAgentHandle) types.UserMessage {
	h.mu.Lock()
	output, err, status := h.result.Output, h.err, h.status
	h.mu.Unlock()
	body := output
	if err != nil {
		body = "error: " + err.Error()
	}
	body = strings.ReplaceAll(body, "</subagent_result", "<\\/subagent_result")
	text := fmt.Sprintf("<subagent_result handle=%q name=%q status=%q>\n%s\n</subagent_result>", h.id, h.name, status, body)
	return types.NewUserMessage(text)
}

// awaitSpawnedAtFinish runs where a run with background children would
// finish. It waits for the next child to finish, then appends pending
// submissions and every finished result, and reports whether anything was
// appended, which resumes the run.
func (a *Agent) awaitSpawnedAtFinish(ctx context.Context, stream *EventStream, tr *tree.Tree, branch types.BranchID) (bool, error) {
	if err := stream.spawns.waitFinished(ctx); err != nil {
		return false, err
	}
	subs := stream.inbox.takeOpen()
	if err := a.injectSubmissions(ctx, stream, tr, branch, subs); err != nil {
		return false, err
	}
	n, err := a.injectSpawned(ctx, stream, tr, branch)
	return len(subs)+n > 0, err
}
