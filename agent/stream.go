package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/agent/workspace"
)

// Marker resolution errors.
var (
	// ErrUnknownMarker means no tool call with that ID is waiting for a
	// decision: the ID is wrong, or the call already finished or was cancelled.
	ErrUnknownMarker = errors.New("no pending marker for tool call")
	// ErrMarkerResolved means a decision for that tool call was already
	// delivered and has not been consumed yet.
	ErrMarkerResolved = errors.New("marker already resolved")
)

// terminalSendGrace bounds how long a terminal delta (ErrorDelta, DoneDelta)
// waits for a consumer after the stream's context is done. A consumer that is
// still draining after Cancel receives it; one that stopped reading does not
// hold the run goroutine for longer than this.
const terminalSendGrace = 250 * time.Millisecond

// RemoteResolver delivers a marker decision to a stream that runs elsewhere.
type RemoteResolver func(toolCallID string, r Resolution) error

// Resolution holds the consumer's decision for a marked tool call.
type Resolution struct {
	Approved     bool
	ModifiedArgs map[string]any // nil = use original args
	Message      string         // optional reason (shown to LLM on rejection)
	// Approver names who decided, as the host authenticated them. It is
	// recorded with the call (types.CallApproval) and never sent to the model.
	Approver string
	// Grant extends an approval beyond this call when the agent has an
	// ApprovalPolicy: to the tool, to matching arguments, or to the whole
	// conversation, until an optional expiry. nil approves this call only.
	Grant *types.GrantRequest
}

// EventStream is the consumer handle for streaming agent deltas.
type EventStream struct {
	capture      *subAgentCapture
	result       *SubAgentResult
	branch       types.BranchID
	deltas       chan types.Delta
	done         chan struct{}
	err          error
	cancel       context.CancelFunc
	once         sync.Once
	ctx          context.Context
	terminalMu   sync.Mutex
	terminalErr  error
	nonStreaming bool
	// stopToolCallID is the tool call whose result ended the run through
	// AgentConfig.StopAtTools. It is written before the stream closes.
	stopToolCallID string
	// iterations counts the run's model turns, the forced final call
	// included; forced is set when MaxIterForceFinal produced the answer and
	// forcedReason names the limit. Written before the stream closes.
	iterations   int
	forced       bool
	forcedReason string
	// artifacts is what this run's tools read and write: the agent's
	// workspace, plus the scratch of every sub-agent the run delegated to.
	// Nil for an agent without sub-agents.
	artifacts *workspace.Layers
	// runID and path identify this run inside a delegation tree: runID is
	// the root run's ID and path lists the tool call IDs from the root run
	// down to this one. Interrupt IDs derive from them.
	runID string
	path  []string
	// interrupts holds this run's pending decisions and copies of those its
	// children forwarded. markers maps the tool call ID a MarkerDelta names
	// to the interrupt it posted.
	interrupts *MemoryInterruptRouter
	markerMu   sync.Mutex
	markers    map[string]string
	// spawns tracks sub-agents this run started in the background. It is
	// nil when the agent registered no spawn or transcript tools.
	spawns *spawnRegistry
	// resolve is set on remote streams; marker decisions are forwarded to it
	// instead of the local resolution table.
	resolve RemoteResolver
	// inbox holds messages submitted while the run is active. It is nil for
	// streams that cannot take messages (replays, remote and durable runs).
	inbox *inbox
	// released is closed when the run frees its branch claim.
	released chan struct{}
	// claim holds the branches of a local run, nil for other streams.
	claim *runClaim
	// started reports the submission that started this run, if any.
	started *Submission
	// approvals is the conversation's approval policy state, nil when the
	// agent has no ApprovalPolicy.
	approvals *approvalState
}

func newEventStream(ctx context.Context, cancel context.CancelFunc) *EventStream {
	return &EventStream{
		deltas:     make(chan types.Delta, 128),
		done:       make(chan struct{}),
		cancel:     cancel,
		ctx:        ctx,
		runID:      types.NewID(),
		interrupts: NewMemoryInterruptRouter(),
	}
}

// NewRemoteStream adapts a run executing elsewhere (another process, a queue
// worker, a network peer) to an EventStream, so callers handle local and
// remote runs the same way.
//
// deltas must be closed by the producer when the run ends. wait is called
// after deltas closes and returns the run's terminal error; nil wait means
// success. cancel asks the remote run to stop and may be nil. resolve
// forwards marker decisions and may be nil, in which case ResolveMarkerErr
// returns ErrUnknownMarker. As with local streams, a cancelled run's Wait
// error matches ErrStreamCanceled.
func NewRemoteStream(deltas <-chan types.Delta, wait func() error, cancel func(), resolve RemoteResolver) *EventStream {
	ctx, ctxCancel := context.WithCancel(context.Background())
	s := newEventStream(ctx, func() {
		ctxCancel()
		if cancel != nil {
			cancel()
		}
	})
	s.resolve = resolve
	go func() {
		defer ctxCancel()
		for d := range deltas {
			s.send(d)
		}
		var err error
		if wait != nil {
			err = wait()
		}
		s.close(err)
	}()
	return s
}

// Deltas returns a channel that yields deltas. Closed on completion.
func (s *EventStream) Deltas() <-chan types.Delta {
	return s.deltas
}

// Wait blocks until the stream is done and returns any error. When the run
// was cancelled, errors.Is(err, types.ErrStreamCanceled) is true.
func (s *EventStream) Wait() error {
	<-s.done
	return s.err
}

// Cancel stops the stream.
func (s *EventStream) Cancel() {
	s.once.Do(func() {
		s.cancel()
	})
}

// send delivers d to the consumer. Once the stream's context is done,
// ordinary deltas are dropped so a departed consumer cannot block the run,
// but terminal deltas still get a short grace period so a consumer draining
// after Cancel sees how the run ended.
func (s *EventStream) send(d types.Delta) {
	if ed, ok := d.(types.ErrorDelta); ok {
		ed.Error = canceledError(ed.Error)
		d = ed
	}
	select {
	case s.deltas <- d:
		return
	case <-s.ctx.Done():
	}
	if !isTerminalDelta(d) {
		return
	}
	timer := time.NewTimer(terminalSendGrace)
	defer timer.Stop()
	select {
	case s.deltas <- d:
	case <-timer.C:
	}
}

func isTerminalDelta(d types.Delta) bool {
	switch d.(type) {
	case types.ErrorDelta, types.DoneDelta:
		return true
	default:
		return false
	}
}

// canceledError makes every cancellation error match ErrStreamCanceled while
// still matching context.Canceled when that was the cause.
func canceledError(err error) error {
	if err == nil || errors.Is(err, types.ErrStreamCanceled) || !errors.Is(err, context.Canceled) {
		return err
	}
	return fmt.Errorf("%w: %w", types.ErrStreamCanceled, err)
}

func (s *EventStream) close(err error) {
	s.err = canceledError(err)
	close(s.deltas)
	close(s.done)
}

// ResolveMarkerErr delivers the consumer's decision for a marked tool call
// and reports whether it was accepted. It returns ErrUnknownMarker when no
// call with that ID is waiting (a typo, or a call that already finished or
// was cancelled), ErrMarkerResolved when a decision is already pending, and
// types.ErrInterruptExpired when the wait ended at its deadline.
func (s *EventStream) ResolveMarkerErr(toolCallID string, r Resolution) error {
	if r.Approved && r.Grant != nil {
		if err := r.Grant.Validate(); err != nil {
			return err
		}
	}
	return s.replyMarker(toolCallID, types.InterruptReply{
		Decision: types.ApprovalDecision{Approved: r.Approved, ModifiedArgs: r.ModifiedArgs, Message: r.Message, Approver: r.Approver, Grant: r.Grant},
	})
}

// replyMarker answers the interrupt the MarkerDelta for toolCallID posted.
func (s *EventStream) replyMarker(toolCallID string, reply types.InterruptReply) error {
	if s.resolve != nil {
		d := reply.Decision
		if d.Message == "" && len(reply.Answer) > 0 {
			d.Message = replyAnswer(reply)
		}
		return s.resolve(toolCallID, Resolution{Approved: d.Approved, ModifiedArgs: d.ModifiedArgs, Message: d.Message, Approver: d.Approver, Grant: d.Grant})
	}
	s.markerMu.Lock()
	id, ok := s.markers[toolCallID]
	s.markerMu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownMarker, toolCallID)
	}
	reply.ID = id
	err := s.interrupts.Reply(context.Background(), reply)
	if errors.Is(err, types.ErrInterruptNotFound) {
		return fmt.Errorf("%w: %s", ErrUnknownMarker, toolCallID)
	}
	return err
}

// ReplyInterrupt answers a pending interrupt by its ID, as listed by
// PendingInterrupts or carried on MarkerDelta.Interrupt. An approval reads
// Decision. A clarification reads Answer, a JSON string or any JSON value,
// and falls back to Decision.Message. Replies are idempotent by
// (ID, IdempotencyKey). Remote streams return types.ErrNoInterruptRouter;
// answer them with ResolveMarkerErr.
func (s *EventStream) ReplyInterrupt(ctx context.Context, reply types.InterruptReply) error {
	if s.resolve != nil {
		return types.ErrNoInterruptRouter
	}
	return s.interrupts.Reply(ctx, reply)
}

// PendingInterrupts lists the decisions this run is waiting for, including
// those raised by its delegated and spawned children, oldest first.
func (s *EventStream) PendingInterrupts() []types.Interrupt {
	out, _ := s.interrupts.Pending(context.Background(), "")
	return out
}

// ResolveMarker provides the consumer's decision for a marked tool call.
// Call this in response to a MarkerDelta to unblock tool execution. A
// decision that cannot be delivered is logged; use ResolveMarkerErr to
// handle that case.
func (s *EventStream) ResolveMarker(toolCallID string, approved bool, modifiedArgs map[string]any) {
	s.logResolveError(s.ResolveMarkerErr(toolCallID, Resolution{Approved: approved, ModifiedArgs: modifiedArgs}))
}

// ResolveMarkerWithMessage provides the consumer's decision with an optional
// message. Like ResolveMarker, it logs a decision that cannot be delivered.
func (s *EventStream) ResolveMarkerWithMessage(toolCallID string, approved bool, modifiedArgs map[string]any, message string) {
	s.logResolveError(s.ResolveMarkerErr(toolCallID, Resolution{Approved: approved, ModifiedArgs: modifiedArgs, Message: message}))
}

func (s *EventStream) logResolveError(err error) {
	if err != nil {
		slog.Default().Warn("marker resolution not delivered", "error", err)
	}
}

// newInterrupt builds this run's interrupt for one decision about the call
// callID. phase separates decisions about the same call, such as a gate and
// a marker, so each gets its own ID.
func (s *EventStream) newInterrupt(kind types.InterruptKind, phase, callID string, markers []types.Marker, payload []byte, ttl time.Duration, policy types.InterruptPolicy) types.Interrupt {
	now := time.Now().UTC()
	in := types.Interrupt{
		ID:        types.InterruptID(s.runID, s.path, phase, callID),
		RunID:     s.runID,
		Path:      append([]string(nil), s.path...),
		Kind:      kind,
		Payload:   payload,
		Markers:   markers,
		CreatedAt: now,
		Policy:    policy,
	}
	if ttl > 0 {
		in.ExpiresAt = now.Add(ttl)
	}
	return in
}

// postInterrupt registers in under the tool call ID its MarkerDelta names.
// The registration happens before the marker is sent, so a consumer that
// answers at once is never told the marker is unknown.
func (s *EventStream) postInterrupt(in types.Interrupt, markerID string) (<-chan types.InterruptReply, error) {
	reply, err := s.interrupts.Post(context.Background(), in)
	if err != nil {
		return nil, err
	}
	s.markerMu.Lock()
	if s.markers == nil {
		s.markers = make(map[string]string)
	}
	s.markers[markerID] = in.ID
	s.markerMu.Unlock()
	return reply, nil
}

// withdrawInterrupt ends the wait for the marker markerID. A later decision
// for an unanswered marker returns ErrUnknownMarker. An answered marker
// stays addressable, so a retried decision with the same idempotency key
// returns nil.
func (s *EventStream) withdrawInterrupt(markerID string) {
	s.markerMu.Lock()
	defer s.markerMu.Unlock()
	id, ok := s.markers[markerID]
	if !ok {
		return
	}
	if !s.interrupts.withdraw(id) {
		delete(s.markers, markerID)
	}
}

// ── Replay ──────────────────────────────────────────────────────────

// Replay converts stored messages into a stream of deltas, enabling
// session restoration. Clients receive the same delta types as if the
// conversation happened live. Only assistant messages and tool results
// produce deltas: system and user text messages are context, not events.
func Replay(messages []types.Message) *EventStream {
	ctx, cancel := context.WithCancel(context.Background())
	stream := newEventStream(ctx, cancel)

	go func() {
		defer func() {
			stream.send(types.DoneDelta{})
			stream.close(nil)
		}()

		// Tool results carry only the call ID; the name comes from the
		// matching tool use earlier in the transcript.
		names := make(map[string]string)
		for _, msg := range messages {
			switch v := msg.(type) {
			case types.AssistantMessage:
				for _, c := range v.Content {
					if tu, ok := c.(types.ToolUseContent); ok {
						names[tu.ID] = tu.Name
					}
				}
				replayAssistantBlocks(stream, v)
			case types.SystemMessage:
				replayToolResults(stream, v.Content, names)
			case types.UserMessage:
				replayUserToolResults(stream, v.Content, names)
			}
		}
	}()

	return stream
}

// replayAssistantBlocks emits an AssistantMessage's content as live deltas. It
// is shared by Replay and by the durable LLM step's replay path so a memoized
// (non-streamed) assistant message still produces a consistent delta sequence.
func replayAssistantBlocks(stream *EventStream, msg types.AssistantMessage) {
	for _, c := range msg.Content {
		switch bc := c.(type) {
		case types.ThinkingContent:
			stream.send(types.ThinkingStartDelta{})
			stream.send(types.ThinkingContentDelta{Content: bc.Thinking})
			stream.send(types.ThinkingEndDelta{Signature: bc.Signature})
		case types.TextContent:
			stream.send(types.TextStartDelta{})
			stream.send(types.TextContentDelta{Content: bc.Text})
			stream.send(types.TextEndDelta{})
		case types.ToolUseContent:
			stream.send(types.ToolCallStartDelta{ID: bc.ID, Name: bc.Name})
			stream.send(types.ToolCallEndDelta{ID: bc.ID, Arguments: bc.Arguments, ArgumentsError: bc.ArgumentsError})
		}
	}
}

func replayToolResults(stream *EventStream, content []types.SystemContent, names map[string]string) {
	for _, c := range content {
		switch v := c.(type) {
		case types.ToolResultContent:
			replayToolResult(stream, v, names)
		case types.HandoffContent:
			stream.send(types.HandoffDelta{From: v.From, To: v.To, Reason: v.Reason})
		}
	}
}

func replayUserToolResults(stream *EventStream, content []types.UserContent, names map[string]string) {
	for _, c := range content {
		switch v := c.(type) {
		case types.ToolResultContent:
			replayToolResult(stream, v, names)
		case types.FeedbackContent:
			stream.send(types.FeedbackDelta(v))
		case types.HandoffContent:
			stream.send(types.HandoffDelta{From: v.From, To: v.To, Reason: v.Reason})
		}
	}
}

// replayToolResult emits the start and end deltas for one stored result.
func replayToolResult(stream *EventStream, v types.ToolResultContent, names map[string]string) {
	name := names[v.ToolCallID]
	stream.send(types.ToolExecStartDelta{ToolCallID: v.ToolCallID, Name: name})
	stream.send(types.ToolExecEndDelta{ToolCallID: v.ToolCallID, Name: name, Result: v.Text, Blocks: v.Blocks})
}

func (s *EventStream) stopRun(err error) {
	s.terminalMu.Lock()
	defer s.terminalMu.Unlock()
	if s.terminalErr == nil || errors.Is(s.terminalErr, types.ErrSuspended) {
		s.terminalErr = err
	}
}
func (s *EventStream) runError() error {
	s.terminalMu.Lock()
	defer s.terminalMu.Unlock()
	return s.terminalErr
}

// StopToolCallID returns the ID of the tool call that ended the run through
// AgentConfig.StopAtTools, or "" when the run ended another way. The call's
// result is the last tool result on the run's branch. Read it after Wait.
func (s *EventStream) StopToolCallID() string {
	select {
	case <-s.done:
		return s.stopToolCallID
	default:
		return ""
	}
}
