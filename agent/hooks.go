package agent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// HookEvent names a point in a run where hooks are called.
type HookEvent string

// Hook events, in the order a run meets them.
const (
	HookRunStart          HookEvent = "run_start"
	HookUserInput         HookEvent = "user_input"
	HookBeforeCompaction  HookEvent = "before_compaction"
	HookAfterCompaction   HookEvent = "after_compaction"
	HookBeforeModelCall   HookEvent = "before_model_call"
	HookAfterModelCall    HookEvent = "after_model_call"
	HookBeforeTool        HookEvent = "before_tool"
	HookAfterTool         HookEvent = "after_tool"
	HookSubagentStart     HookEvent = "subagent_start"
	HookSubagentEnd       HookEvent = "subagent_end"
	HookInterruptRaised   HookEvent = "interrupt_raised"
	HookInterruptResolved HookEvent = "interrupt_resolved"
	HookTurnEnd           HookEvent = "turn_end"
	HookRunStop           HookEvent = "run_stop"
)

// DefaultHookTimeout bounds each hook and guardrail call when
// AgentConfig.HookTimeout is zero.
const DefaultHookTimeout = 30 * time.Second

// Hooks is one set of run-lifecycle callbacks. Every field is optional.
//
// Hooks observe a run, and at the points where it is safe they may change
// the event or abort the run. The contract:
//
//   - Order. Hook sets run in the order they were added, a sub-agent's
//     inherited sets before its own. Within a point, each hook sees the
//     changes of the hooks before it. The first abort stops the chain.
//   - Changes. Only the fields an event documents as changeable are read
//     back: UserInputEvent.Message, BeforeToolEvent.Arguments,
//     AfterToolEvent.Result and Error, CompactionEvent.Skip (before) and
//     SubagentStartEvent.Task. Changes to any other field are ignored.
//   - Abort. At RunStart, UserInput, BeforeModelCall, BeforeTool,
//     AfterTool, BeforeCompaction, SubagentStart and TurnEnd any error a
//     hook returns stops the run with a *HookAbortError matching
//     ErrHookAborted. Use Abort to give a reason. A tool call that is
//     aborted still gets an error result, so every tool_use keeps its
//     tool_result. The other events only observe: an error is logged and
//     the run goes on.
//   - Timeouts and panics. Each call gets a context bounded by
//     AgentConfig.HookTimeout (DefaultHookTimeout when zero, none when
//     negative). A hook that returns after its deadline, or panics, has
//     failed: an abortable point aborts, an observing one logs. The agent
//     waits for a hook to return rather than abandon it, because a hook
//     still running would race with the run; a hook must honor its context.
//   - Durable replay. Under a durable StepRunner the outcome of every
//     abortable or changing point is recorded as a step (types.StepKindHook),
//     so a replay applies the recorded outcome without calling the hooks
//     again. Observing hooks are called again on replay;
//     AfterModelCallEvent.Replayed tells a replayed call apart.
//   - Concurrency. Tool, sub-agent and interrupt hooks of one turn may run
//     at the same time, from the goroutines that run the calls, so a hook
//     set must be safe for concurrent use.
//
// Hooks run inside the loop; they never add or remove messages on their own,
// so they cannot split a tool call from its result or move a safe point.
type Hooks struct {
	// Name identifies the set in logs and abort errors.
	Name string

	RunStart          func(context.Context, *RunStartEvent) error
	UserInput         func(context.Context, *UserInputEvent) error
	BeforeCompaction  func(context.Context, *CompactionEvent) error
	AfterCompaction   func(context.Context, *CompactionEvent) error
	BeforeModelCall   func(context.Context, *BeforeModelCallEvent) error
	AfterModelCall    func(context.Context, *AfterModelCallEvent) error
	BeforeTool        func(context.Context, *BeforeToolEvent) error
	AfterTool         func(context.Context, *AfterToolEvent) error
	SubagentStart     func(context.Context, *SubagentStartEvent) error
	SubagentEnd       func(context.Context, *SubagentEndEvent) error
	InterruptRaised   func(context.Context, *InterruptEvent) error
	InterruptResolved func(context.Context, *InterruptEvent) error
	TurnEnd           func(context.Context, *TurnEndEvent) error
	RunStop           func(context.Context, *RunStopEvent) error
}

// WithHooks adds hook sets to the agent. They run after any set added
// before, including those a sub-agent inherits.
func WithHooks(hooks ...Hooks) AgentOption {
	return func(c *AgentConfig) { c.Hooks = append(c.Hooks, hooks...) }
}

// WithHookTimeout bounds each hook and guardrail call. Zero uses
// DefaultHookTimeout; a negative value removes the bound.
func WithHookTimeout(d time.Duration) AgentOption {
	return func(c *AgentConfig) { c.HookTimeout = d }
}

// HookRun identifies the run an event belongs to.
type HookRun struct {
	// Agent is the agent that owns the turn: the active handoff member, or
	// the agent itself.
	Agent string
	// RunID is the root run's ID, shared by every run of a delegation tree.
	RunID string
	// Path lists the tool call IDs from the root run down to this run. It is
	// empty for a root run.
	Path   []string
	Branch types.BranchID
}

// RunStartEvent is sent once, before the run's input is appended.
type RunStartEvent struct {
	HookRun
	// Input is the run's input, read-only.
	Input []types.Message
}

// UserInputEvent is sent for each user message before it is appended: the
// run's input and every message submitted while it runs.
type UserInputEvent struct {
	HookRun
	// Source is "input" for the run's input, or the submission mode ("queue",
	// "steer", "interrupt") of a submitted message.
	Source string
	// Message may be replaced, for example to annotate or redact it.
	Message types.UserMessage
}

// CompactionEvent is sent before and after the run tries to compact its
// history. With a message-count compactor that is before every turn, and
// the compactor itself decides whether anything changes: AfterCompaction
// reports whether it did.
type CompactionEvent struct {
	HookRun
	// Forced is true when the turn must shrink whatever its size: after a
	// context-length error or an explicit request.
	Forced bool
	// Messages is the number of messages sent with the turn.
	Messages int
	// Skip, set by a BeforeCompaction hook, keeps the full history this time.
	Skip bool
	// Compacted and NewBranch report the result to AfterCompaction.
	Compacted bool
	NewBranch types.BranchID
}

// BeforeModelCallEvent is sent before each model call of the loop.
type BeforeModelCallEvent struct {
	HookRun
	// Step is the call's durable step name.
	Step     string
	Provider string
	Model    string
	// Messages, Tools and Options are the request, read-only. Options is nil
	// when the call carries no per-request controls.
	Messages []types.Message
	Tools    []types.ToolDef
	Options  *types.RequestOptions
}

// AfterModelCallEvent is sent after each model call, also a failed one.
type AfterModelCallEvent struct {
	HookRun
	Step     string
	Provider string
	Model    string
	Options  *types.RequestOptions
	// Message is the aggregated turn; nil when the call failed or returned
	// nothing.
	Message *types.AssistantMessage
	Usage   types.UsageDelta
	// Dials reports how the call's dials compiled, nil when it had none.
	Dials *types.DialReport
	Err   error
	// Replayed is true when a durable runner returned the recorded turn
	// without calling the provider.
	Replayed bool
}

// BeforeToolEvent is sent for each tool call after its gate and any approval
// cleared it, just before it runs.
type BeforeToolEvent struct {
	HookRun
	Call types.ToolUseContent
	Tool types.ToolDef
	// Arguments may be replaced. They are validated against the tool's
	// schema again, but not gated again. Replace the map rather than mutate
	// nested values.
	Arguments map[string]any
}

// AfterToolEvent is sent after a tool ran, before its result is streamed and
// recorded. Values a ToolRedactor tokenized are placeholders here.
type AfterToolEvent struct {
	HookRun
	Call types.ToolUseContent
	Tool types.ToolDef
	// Result and Error may be replaced. Blocks are read-only.
	Result string
	Blocks []types.ToolResultBlock
	Error  string
}

// SubagentStartEvent is sent before a sub-agent starts, for a delegation or
// a spawn.
type SubagentStartEvent struct {
	HookRun
	CallID string
	Name   string
	// Mode is "delegate" or "spawn".
	Mode string
	// Task may be replaced.
	Task string
}

// SubagentEndEvent is sent when a sub-agent finished. For a spawn it is sent
// from the goroutine that waited for the child.
type SubagentEndEvent struct {
	HookRun
	CallID string
	Name   string
	Mode   string
	Output string
	Err    error
}

// InterruptEvent is sent when the run posts a decision it waits for, and
// again when the decision arrives or lapses.
type InterruptEvent struct {
	HookRun
	Interrupt types.Interrupt
	Call      types.ToolUseContent
	// Approved, Approver and Message describe the decision; they are set for
	// InterruptResolved only.
	Approved bool
	Approver string
	Message  string
}

// TurnEndEvent is sent after a model turn and its tool results are recorded,
// at a safe point.
type TurnEndEvent struct {
	HookRun
	Step    string
	Message types.AssistantMessage
	Results []types.ToolResultContent
	// Final is true for a turn without tool calls, which ends the user turn.
	Final bool
}

// RunStopReason says why a run ended.
type RunStopReason string

// Run stop reasons.
const (
	RunStopCompleted RunStopReason = "completed"
	RunStopTool      RunStopReason = "stop_tool"
	RunStopCanceled  RunStopReason = "canceled"
	RunStopSuspended RunStopReason = "suspended"
	RunStopLimit     RunStopReason = "limit"
	RunStopBudget    RunStopReason = "budget"
	RunStopAborted   RunStopReason = "aborted"
	RunStopGuardrail RunStopReason = "guardrail"
	RunStopFailed    RunStopReason = "error"
)

// RunStopEvent is sent once when the run ends, after its branch claim is
// released and before the stream reports completion. It is the place for
// work after a run, such as extracting memories; it runs even when the run
// was cancelled, with a context that is not.
type RunStopEvent struct {
	HookRun
	Reason RunStopReason
	Err    error
	// Messages is the run's branch when it ended, oldest first.
	Messages []types.Message
	Duration time.Duration
}

// HookAbortError reports a run that a hook stopped. It matches
// ErrHookAborted.
type HookAbortError struct {
	Event  HookEvent
	Hook   string
	Reason string
	// Err is the hook's own error, nil when it used Abort or when the abort
	// was replayed from a durable record.
	Err error
}

func (e *HookAbortError) Error() string {
	who := e.Hook
	if who == "" {
		who = "hook"
	}
	return fmt.Sprintf("%s aborted the run at %s: %s", who, e.Event, e.Reason)
}

// Unwrap returns ErrHookAborted and the hook's own error.
func (e *HookAbortError) Unwrap() []error {
	if e.Err == nil {
		return []error{types.ErrHookAborted}
	}
	return []error{types.ErrHookAborted, e.Err}
}

// ErrHookAborted is matched by the error of a run a hook stopped.
var ErrHookAborted = types.ErrHookAborted

// ErrHookTimeout is matched by the error of a hook that ran past its
// deadline.
var ErrHookTimeout = errors.New("hook timed out")

// Abort returns the error a hook returns to stop the run with reason.
func Abort(reason string) error { return &HookAbortError{Reason: reason} }

// ── Runtime ──────────────────────────────────────────────────────────

func (a *Agent) hookTimeout() time.Duration {
	switch d := a.cfg.HookTimeout; {
	case d == 0:
		return DefaultHookTimeout
	case d < 0:
		return 0
	default:
		return d
	}
}

// callHook runs one hook or guardrail call under the hook timeout and turns
// a panic or a late return into its error.
func (a *Agent) callHook(ctx context.Context, what string, fn func(context.Context) error) (err error) {
	timeout := a.hookTimeout()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	defer func() {
		if p := recover(); p != nil {
			a.cfg.Logger.Error("hook panic recovered", "agent", a.cfg.Name, "hook", what, "panic", p, "stack", string(debug.Stack()))
			err = fmt.Errorf("%s panicked: %v", what, p)
		}
	}()
	err = fn(ctx)
	if timeout > 0 && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = errors.Join(fmt.Errorf("%s: %w after %s", what, ErrHookTimeout, timeout), err)
	}
	return err
}

// hookFn picks one event's callback from a hook set.
type hookFn[E any] func(Hooks) func(context.Context, *E) error

func hasHooks[E any](a *Agent, pick hookFn[E]) bool {
	for _, h := range a.cfg.Hooks {
		if pick(h) != nil {
			return true
		}
	}
	return false
}

// runHooks calls the event's hooks in order on ev, which they may change. It
// stops at the first error and returns it as a *HookAbortError.
func runHooks[E any](ctx context.Context, a *Agent, event HookEvent, pick hookFn[E], ev *E) *HookAbortError {
	for _, h := range a.cfg.Hooks {
		fn := pick(h)
		if fn == nil {
			continue
		}
		if err := a.callHook(ctx, hookName(h), func(ctx context.Context) error { return fn(ctx, ev) }); err != nil {
			return abortError(event, h.Name, err)
		}
	}
	return nil
}

// observeHooks calls the event's hooks in order. Each gets its own copy of
// ev; an error is logged and the next hook still runs.
func observeHooks[E any](ctx context.Context, a *Agent, event HookEvent, pick hookFn[E], ev E) {
	for _, h := range a.cfg.Hooks {
		fn := pick(h)
		if fn == nil {
			continue
		}
		e := ev
		if err := a.callHook(ctx, hookName(h), func(ctx context.Context) error { return fn(ctx, &e) }); err != nil {
			a.cfg.Logger.Warn("hook failed", "agent", a.cfg.Name, "hook", h.Name, "event", event, "error", err)
		}
	}
}

func hookName(h Hooks) string {
	if h.Name == "" {
		return "hook"
	}
	return "hook " + h.Name
}

func abortError(event HookEvent, hook string, err error) *HookAbortError {
	var ab *HookAbortError
	if errors.As(err, &ab) && ab.Err == nil {
		return &HookAbortError{Event: event, Hook: hook, Reason: ab.Reason}
	}
	return &HookAbortError{Event: event, Hook: hook, Reason: err.Error(), Err: err}
}

// abortRecord records err, a *HookAbortError or nil, in r.
func abortRecord(r *types.HookRecord, err *HookAbortError) {
	if err == nil {
		return
	}
	r.Abort, r.Name, r.Reason = true, err.Hook, err.Reason
}

// recordedAbort returns the abort a record holds. live is the error of a run
// that called the hooks, preferred because it keeps the hook's own error.
func recordedAbort(event HookEvent, r types.HookRecord, live *HookAbortError) error {
	if !r.Abort {
		return nil
	}
	if live != nil {
		return live
	}
	return &HookAbortError{Event: event, Hook: r.Name, Reason: r.Reason}
}

// recordHook runs decide, the hooks or guardrails at one point that may
// change the run. Under a durable runner it is a step named step, so a replay
// returns the recorded outcome without calling them. ran reports whether
// decide was called.
func (a *Agent) recordHook(ctx context.Context, step string, decide func(context.Context) types.HookRecord) (rec types.HookRecord, ran bool, err error) {
	if _, inline := a.cfg.StepRunner.(types.NoopStepRunner); inline {
		return decide(ctx), true, nil
	}
	sr, err := a.cfg.StepRunner.RunStep(ctx, step, func(context.Context) (types.StepResult, error) {
		// The run's context, not the step's, carries the run scope the
		// hooks may read.
		ran = true
		r := decide(ctx)
		return types.StepResult{Kind: types.StepKindHook, Hook: &r}, nil
	})
	if err != nil || sr.Hook == nil {
		return types.HookRecord{}, ran, err
	}
	return *sr.Hook, ran, nil
}

// hookStep names the next recorded step of event in this run. Points the
// loop meets in order are numbered; tool points use the call ID instead.
func (s *EventStream) hookStep(event HookEvent) string {
	s.hookMu.Lock()
	defer s.hookMu.Unlock()
	if s.hookSeq == nil {
		s.hookSeq = map[HookEvent]int{}
	}
	n := s.hookSeq[event]
	s.hookSeq[event]++
	return "hook-" + string(event) + "-" + strconv.Itoa(n)
}

func (a *Agent) hookRun(ctx context.Context, stream *EventStream) HookRun {
	// The run scope names the active handoff member, but a sub-agent's
	// context still carries its caller's scope until its first turn.
	name := a.cfg.Name
	if scope, ok := RunScopeFromContext(ctx); ok && scope.Agent != "" {
		if root := a.cfg.Tree.Root(); root != nil && scope.Conversation == root.ID {
			name = scope.Agent
		}
	}
	return HookRun{Agent: name, RunID: stream.runID, Path: slices.Clone(stream.path), Branch: stream.branch}
}

// ── Points ───────────────────────────────────────────────────────────

func pickRunStart(h Hooks) func(context.Context, *RunStartEvent) error { return h.RunStart }
func pickUserInput(h Hooks) func(context.Context, *UserInputEvent) error {
	return h.UserInput
}
func pickBeforeCompaction(h Hooks) func(context.Context, *CompactionEvent) error {
	return h.BeforeCompaction
}
func pickAfterCompaction(h Hooks) func(context.Context, *CompactionEvent) error {
	return h.AfterCompaction
}
func pickBeforeModelCall(h Hooks) func(context.Context, *BeforeModelCallEvent) error {
	return h.BeforeModelCall
}
func pickAfterModelCall(h Hooks) func(context.Context, *AfterModelCallEvent) error {
	return h.AfterModelCall
}
func pickBeforeTool(h Hooks) func(context.Context, *BeforeToolEvent) error { return h.BeforeTool }
func pickAfterTool(h Hooks) func(context.Context, *AfterToolEvent) error   { return h.AfterTool }
func pickSubagentStart(h Hooks) func(context.Context, *SubagentStartEvent) error {
	return h.SubagentStart
}
func pickSubagentEnd(h Hooks) func(context.Context, *SubagentEndEvent) error {
	return h.SubagentEnd
}
func pickInterruptRaised(h Hooks) func(context.Context, *InterruptEvent) error {
	return h.InterruptRaised
}
func pickInterruptResolved(h Hooks) func(context.Context, *InterruptEvent) error {
	return h.InterruptResolved
}
func pickTurnEnd(h Hooks) func(context.Context, *TurnEndEvent) error { return h.TurnEnd }
func pickRunStop(h Hooks) func(context.Context, *RunStopEvent) error { return h.RunStop }

// runStartHooks calls RunStart and returns the abort, if any.
func (a *Agent) runStartHooks(ctx context.Context, stream *EventStream, input []types.Message) error {
	if !hasHooks(a, pickRunStart) {
		return nil
	}
	var live *HookAbortError
	rec, _, err := a.recordHook(ctx, stream.hookStep(HookRunStart), func(ctx context.Context) types.HookRecord {
		var r types.HookRecord
		live = runHooks(ctx, a, HookRunStart, pickRunStart, &RunStartEvent{HookRun: a.hookRun(ctx, stream), Input: input})
		abortRecord(&r, live)
		return r
	})
	if err != nil {
		return err
	}
	return recordedAbort(HookRunStart, rec, live)
}

// userInputHooks calls UserInput on msg and returns the message to append.
func (a *Agent) userInputHooks(ctx context.Context, stream *EventStream, msg types.UserMessage, source string) (types.UserMessage, error) {
	if !hasHooks(a, pickUserInput) {
		return msg, nil
	}
	var live *HookAbortError
	rec, _, err := a.recordHook(ctx, stream.hookStep(HookUserInput), func(ctx context.Context) types.HookRecord {
		ev := &UserInputEvent{HookRun: a.hookRun(ctx, stream), Source: source, Message: cloneUserMessage(msg)}
		live = runHooks(ctx, a, HookUserInput, pickUserInput, ev)
		r := types.HookRecord{Changed: true, Message: &ev.Message}
		abortRecord(&r, live)
		return r
	})
	if err != nil {
		return msg, err
	}
	if err := recordedAbort(HookUserInput, rec, live); err != nil {
		return msg, err
	}
	if rec.Changed && rec.Message != nil {
		msg = *rec.Message
	}
	return msg, nil
}

// hasUserText reports whether msg carries content a person wrote, as
// opposed to tool results or run metadata only.
func hasUserText(msg types.UserMessage) bool {
	for _, c := range msg.Content {
		if _, ok := c.(types.ToolResultContent); ok || types.IsMetadataContent(c) {
			continue
		}
		return true
	}
	return false
}

func userText(msg types.UserMessage) string {
	var b strings.Builder
	for _, c := range msg.Content {
		if t, ok := c.(types.TextContent); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

func cloneUserMessage(m types.UserMessage) types.UserMessage {
	return types.UserMessage{Content: slices.Clone(m.Content)}
}

// beforeCompactionHooks calls BeforeCompaction. skip reports that a hook
// kept the full history.
func (a *Agent) beforeCompactionHooks(ctx context.Context, stream *EventStream, forced bool, messages int) (skip bool, err error) {
	if !hasHooks(a, pickBeforeCompaction) {
		return false, nil
	}
	var live *HookAbortError
	rec, _, err := a.recordHook(ctx, stream.hookStep(HookBeforeCompaction), func(ctx context.Context) types.HookRecord {
		ev := &CompactionEvent{HookRun: a.hookRun(ctx, stream), Forced: forced, Messages: messages}
		live = runHooks(ctx, a, HookBeforeCompaction, pickBeforeCompaction, ev)
		r := types.HookRecord{Skip: ev.Skip}
		abortRecord(&r, live)
		return r
	})
	if err != nil {
		return false, err
	}
	if err := recordedAbort(HookBeforeCompaction, rec, live); err != nil {
		return false, err
	}
	return rec.Skip, nil
}

func (a *Agent) afterCompactionHooks(ctx context.Context, stream *EventStream, forced bool, messages int, compacted bool, branch types.BranchID) {
	if !hasHooks(a, pickAfterCompaction) {
		return
	}
	observeHooks(ctx, a, HookAfterCompaction, pickAfterCompaction, CompactionEvent{
		HookRun: a.hookRun(ctx, stream), Forced: forced, Messages: messages, Compacted: compacted, NewBranch: branch,
	})
}

// beforeModelCallHooks calls BeforeModelCall and returns the abort, if any.
func (a *Agent) beforeModelCallHooks(ctx context.Context, stream *EventStream, provider types.Provider, messages []types.Message, tools []types.ToolDef, opts *types.RequestOptions, step string) error {
	if !hasHooks(a, pickBeforeModelCall) {
		return nil
	}
	var live *HookAbortError
	rec, _, err := a.recordHook(ctx, "hook-"+string(HookBeforeModelCall)+"-"+step, func(ctx context.Context) types.HookRecord {
		var r types.HookRecord
		live = runHooks(ctx, a, HookBeforeModelCall, pickBeforeModelCall, &BeforeModelCallEvent{
			HookRun: a.hookRun(ctx, stream), Step: step,
			Provider: types.ProviderName(provider), Model: types.ProviderModel(provider),
			Messages: messages, Tools: tools, Options: cloneOptions(opts),
		})
		abortRecord(&r, live)
		return r
	})
	if err != nil {
		return err
	}
	return recordedAbort(HookBeforeModelCall, rec, live)
}

func (a *Agent) afterModelCallHooks(ctx context.Context, stream *EventStream, provider types.Provider, opts *types.RequestOptions, step string, msg *types.AssistantMessage, usage *types.UsageDelta, replayed bool, callErr error) {
	if !hasHooks(a, pickAfterModelCall) {
		return
	}
	ev := AfterModelCallEvent{
		HookRun: a.hookRun(ctx, stream), Step: step,
		Provider: types.ProviderName(provider), Model: types.ProviderModel(provider),
		Options: cloneOptions(opts), Message: msg, Err: callErr, Replayed: replayed,
	}
	if usage != nil {
		ev.Usage = *usage
	}
	if msg != nil {
		for _, c := range msg.Content {
			if rc, ok := c.(types.RouteContent); ok && rc.Dials != nil {
				d := rc.Dials.Clone()
				ev.Dials = &d
			}
		}
	}
	observeHooks(ctx, a, HookAfterModelCall, pickAfterModelCall, ev)
}

func cloneOptions(opts *types.RequestOptions) *types.RequestOptions {
	if opts == nil {
		return nil
	}
	o := opts.Clone()
	return &o
}

// beforeToolHooks calls BeforeTool and may change tc.Arguments. done reports
// that the call is finished with res: an abort, or arguments a hook changed
// that no longer fit the schema.
func (a *Agent) beforeToolHooks(ctx context.Context, stream *EventStream, tc *types.ToolUseContent, def types.ToolDef) (res toolResult, done bool) {
	if !hasHooks(a, pickBeforeTool) {
		return toolResult{}, false
	}
	var live *HookAbortError
	rec, _, err := a.recordHook(ctx, "hook-"+string(HookBeforeTool)+"-"+tc.ID, func(ctx context.Context) types.HookRecord {
		ev := &BeforeToolEvent{HookRun: a.hookRun(ctx, stream), Call: *tc, Tool: def, Arguments: maps.Clone(tc.Arguments)}
		live = runHooks(ctx, a, HookBeforeTool, pickBeforeTool, ev)
		r := types.HookRecord{Changed: true, Arguments: ev.Arguments}
		abortRecord(&r, live)
		return r
	})
	if err == nil {
		err = recordedAbort(HookBeforeTool, rec, live)
	}
	if err != nil {
		stream.stopRun(err)
		return failedTool(stream, tc.ID, tc.Name, err.Error()), true
	}
	if rec.Changed {
		tc.Arguments = rec.Arguments
		if err := types.ValidateToolArgs(def.Parameters, tc.Arguments); err != nil {
			return failedTool(stream, tc.ID, tc.Name, err.Error()), true
		}
	}
	return toolResult{}, false
}

// afterToolHooks calls AfterTool on a finished call and may change its
// result and error.
func (a *Agent) afterToolHooks(ctx context.Context, stream *EventStream, tc types.ToolUseContent, def types.ToolDef, res *toolResult) {
	if !hasHooks(a, pickAfterTool) {
		return
	}
	var live *HookAbortError
	rec, _, err := a.recordHook(ctx, "hook-"+string(HookAfterTool)+"-"+tc.ID, func(ctx context.Context) types.HookRecord {
		ev := &AfterToolEvent{HookRun: a.hookRun(ctx, stream), Call: tc, Tool: def, Result: res.result, Blocks: res.blocks, Error: res.err}
		live = runHooks(ctx, a, HookAfterTool, pickAfterTool, ev)
		r := types.HookRecord{Changed: true, Text: ev.Result, Error: ev.Error}
		abortRecord(&r, live)
		return r
	})
	if err == nil {
		err = recordedAbort(HookAfterTool, rec, live)
	}
	if err != nil {
		// The result is still recorded, so the call keeps its tool_result.
		stream.stopRun(err)
		return
	}
	if rec.Changed {
		res.result, res.err = rec.Text, rec.Error
	}
}

// subagentStartHooks calls SubagentStart and returns the task to send.
func (a *Agent) subagentStartHooks(ctx context.Context, stream *EventStream, callID, name, mode, task string) (string, error) {
	if !hasHooks(a, pickSubagentStart) {
		return task, nil
	}
	var live *HookAbortError
	rec, _, err := a.recordHook(ctx, "hook-"+string(HookSubagentStart)+"-"+callID, func(ctx context.Context) types.HookRecord {
		ev := &SubagentStartEvent{HookRun: a.hookRun(ctx, stream), CallID: callID, Name: name, Mode: mode, Task: task}
		live = runHooks(ctx, a, HookSubagentStart, pickSubagentStart, ev)
		r := types.HookRecord{Changed: true, Text: ev.Task}
		abortRecord(&r, live)
		return r
	})
	if err == nil {
		err = recordedAbort(HookSubagentStart, rec, live)
	}
	if err != nil {
		return task, err
	}
	if rec.Changed {
		task = rec.Text
	}
	return task, nil
}

func (a *Agent) subagentEndHooks(ctx context.Context, stream *EventStream, callID, name, mode, output string, err error) {
	if !hasHooks(a, pickSubagentEnd) {
		return
	}
	observeHooks(ctx, a, HookSubagentEnd, pickSubagentEnd, SubagentEndEvent{
		HookRun: a.hookRun(ctx, stream), CallID: callID, Name: name, Mode: mode, Output: output, Err: err,
	})
}

func (a *Agent) interruptHooks(ctx context.Context, stream *EventStream, event HookEvent, in types.Interrupt, call types.ToolUseContent, d *decision, approved bool) {
	pick := pickInterruptRaised
	if event == HookInterruptResolved {
		pick = pickInterruptResolved
	}
	if !hasHooks(a, pick) {
		return
	}
	ev := InterruptEvent{HookRun: a.hookRun(ctx, stream), Interrupt: in, Call: call}
	if d != nil {
		ev.Approved, ev.Approver, ev.Message = approved, d.approver, d.message
	}
	observeHooks(ctx, a, event, pick, ev)
}

// turnEndHooks calls TurnEnd at the safe point after a turn and returns the
// abort, if any.
func (a *Agent) turnEndHooks(ctx context.Context, stream *EventStream, step string, msg types.AssistantMessage, results []toolResult) error {
	if !hasHooks(a, pickTurnEnd) {
		return nil
	}
	var live *HookAbortError
	rec, _, err := a.recordHook(ctx, "hook-"+string(HookTurnEnd)+"-"+step, func(ctx context.Context) types.HookRecord {
		var r types.HookRecord
		live = runHooks(ctx, a, HookTurnEnd, pickTurnEnd, &TurnEndEvent{
			HookRun: a.hookRun(ctx, stream), Step: step, Message: msg, Results: resultContents(results), Final: len(results) == 0,
		})
		abortRecord(&r, live)
		return r
	})
	if err != nil {
		return err
	}
	return recordedAbort(HookTurnEnd, rec, live)
}

func resultContents(results []toolResult) []types.ToolResultContent {
	if len(results) == 0 {
		return nil
	}
	out := make([]types.ToolResultContent, len(results))
	for i, r := range results {
		out[i] = types.ToolResultContent{ToolCallID: r.toolCallID, Text: r.result, Blocks: r.blocks, IsError: r.err != "", ToolVersion: r.version}
		if r.err != "" && out[i].Text == "" {
			out[i].Text = r.err
		}
	}
	return out
}

// runStopHooks calls RunStop with the run's outcome. It runs on a context
// that the run's cancellation does not reach.
func (a *Agent) runStopHooks(ctx context.Context, stream *EventStream, messages []types.Message, started time.Time, runErr error) {
	if !hasHooks(a, pickRunStop) {
		return
	}
	ctx = context.WithoutCancel(ctx)
	observeHooks(ctx, a, HookRunStop, pickRunStop, RunStopEvent{
		HookRun: a.hookRun(ctx, stream), Reason: stopReason(stream, runErr), Err: runErr,
		Messages: messages, Duration: time.Since(started),
	})
}

func stopReason(stream *EventStream, err error) RunStopReason {
	switch {
	case err == nil && stream.stopToolCallID != "":
		return RunStopTool
	case err == nil:
		return RunStopCompleted
	case errors.Is(err, types.ErrGuardrailTripped):
		return RunStopGuardrail
	case errors.Is(err, types.ErrHookAborted):
		return RunStopAborted
	case errors.Is(err, types.ErrSuspended):
		return RunStopSuspended
	case errors.Is(err, types.ErrStreamCanceled), errors.Is(err, context.Canceled):
		return RunStopCanceled
	case errors.Is(err, types.ErrBudgetExceeded), errors.Is(err, types.ErrBudgetAdmission):
		return RunStopBudget
	case errors.Is(err, types.ErrMaxIterations), errors.Is(err, ErrToolErrorLimit), errors.Is(err, ErrRepeatedToolCalls):
		return RunStopLimit
	default:
		return RunStopFailed
	}
}
