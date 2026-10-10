package agent

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"

	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
)

// SubmitMode chooses how a message sent during an active run joins it.
type SubmitMode int

const (
	// SubmitQueue holds the message until the run would otherwise finish,
	// then appends it as a new user turn. The same stream continues.
	SubmitQueue SubmitMode = iota
	// SubmitSteer appends the message at the next safe point, after any tool
	// results the current turn produces, without cancelling anything. The
	// model sees it on its next call.
	SubmitSteer
	// SubmitInterruptReplace stops the in-flight provider call. The completed
	// text of that turn is committed with a TruncationPart marker, its tool
	// calls are dropped, and the message is appended in its place. Tools that
	// are already running finish first; their results are kept.
	SubmitInterruptReplace
	// SubmitSide branches from the last safe point of the branch and runs the
	// message on a separate stream. The side run never writes to the original
	// branch. Only Agent.Submit accepts it.
	SubmitSide
)

// String returns the mode's wire name, as used by QueuedDelta and
// InjectedDelta.
func (m SubmitMode) String() string {
	switch m {
	case SubmitQueue:
		return "queue"
	case SubmitSteer:
		return "steer"
	case SubmitInterruptReplace:
		return "interrupt"
	case SubmitSide:
		return "side"
	default:
		return fmt.Sprintf("SubmitMode(%d)", int(m))
	}
}

// SubmissionID identifies one submitted message.
type SubmissionID string

// Submission is a message accepted by a run but not yet appended.
type Submission struct {
	ID      SubmissionID
	Mode    SubmitMode
	Message types.UserMessage
}

// Submission errors.
var (
	// ErrRunFinished means the run has passed its last safe point; a new
	// message needs a new run. Agent.Submit starts one automatically.
	ErrRunFinished = errors.New("run has finished")
	// ErrSubmitUnsupported means the stream cannot take messages: it replays
	// stored history, runs elsewhere, or belongs to a run with no consumer.
	ErrSubmitUnsupported = errors.New("stream does not accept submissions")
	// ErrInvalidSubmission means the message or mode cannot be submitted.
	ErrInvalidSubmission = errors.New("invalid submission")
	// ErrNothingToContinue means Continue found no assistant turn to resume.
	ErrNothingToContinue = errors.New("branch does not end with an assistant turn")
)

// DefaultContinuePrompt is the user message Continue and auto-continue send
// when the provider cannot resume a partial assistant turn directly.
const DefaultContinuePrompt = "Continue exactly where you stopped. Do not repeat what you already wrote."

// errInterruptRequested is the cancellation cause of a provider call stopped
// by SubmitInterruptReplace.
var errInterruptRequested = errors.New("interrupted by a submitted message")

// errTurnInterrupted wraps context.Canceled for a provider call stopped by
// SubmitInterruptReplace, so durable runners can record the partial turn the
// step returns alongside it.
var errTurnInterrupted = fmt.Errorf("%w: %w", errInterruptRequested, context.Canceled)

// inbox holds the submissions of one run and the cancel function of the
// provider call in flight, if any.
type inbox struct {
	mu      sync.Mutex
	pending []Submission
	closed  bool
	// cancelTurn stops the provider call in flight; nil between calls.
	cancelTurn context.CancelCauseFunc
	// interruptedBy is the submission that stopped the current call.
	interruptedBy SubmissionID
	// acks holds QueuedDeltas that Submit could not send without waiting.
	// The run sends them at its next safe point, before any InjectedDelta.
	acks []types.QueuedDelta
}

// Submit sends msg to the active run behind this stream. The run
// acknowledges it with a QueuedDelta and reports an InjectedDelta when the
// message is appended to the branch. The QueuedDelta for a submission always
// precedes its InjectedDelta.
//
// It returns ErrRunFinished once the run has passed its last safe point and
// ErrSubmitUnsupported for streams that cannot take messages, which include
// runs under a durable step runner. SubmitSide starts a separate stream, so
// it is only accepted by Agent.Submit. Submit never waits for the consumer,
// so it is safe to call from the goroutine that reads Deltas.
func (s *EventStream) Submit(msg types.UserMessage, mode SubmitMode) (SubmissionID, error) {
	if err := validateSubmission(msg, mode); err != nil {
		return "", err
	}
	if mode == SubmitSide {
		return "", fmt.Errorf("%w: side submissions start their own stream; use Agent.Submit", ErrSubmitUnsupported)
	}
	in := s.inbox
	if in == nil {
		return "", ErrSubmitUnsupported
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return "", ErrRunFinished
	}
	sub := Submission{ID: newSubmissionID(), Mode: mode, Message: msg}
	in.pending = append(in.pending, sub)
	if mode == SubmitInterruptReplace && in.cancelTurn != nil && in.interruptedBy == "" {
		in.interruptedBy = sub.ID
		in.cancelTurn(errInterruptRequested)
	}
	// The acknowledgement is offered under the lock, so it precedes the
	// InjectedDelta for the same submission. When the buffer is full, or
	// earlier acknowledgements still wait, the run sends it instead. The
	// lock also keeps the run from closing the delta channel meanwhile.
	ack := types.QueuedDelta{SubmissionID: string(sub.ID), Mode: mode.String(), Position: len(in.pending)}
	if len(in.acks) > 0 || !s.trySend(ack) {
		in.acks = append(in.acks, ack)
	}
	return sub.ID, nil
}

// trySend delivers d only if the consumer has buffer room.
func (s *EventStream) trySend(d types.Delta) bool {
	select {
	case s.deltas <- d:
		return true
	default:
		return false
	}
}

// flushAcks sends the acknowledgements Submit left to the run. It runs on
// the run goroutine, which may wait for the consumer.
func (s *EventStream) flushAcks() {
	in := s.inbox
	if in == nil {
		return
	}
	in.mu.Lock()
	acks := in.acks
	in.acks = nil
	in.mu.Unlock()
	for _, d := range acks {
		s.send(d)
	}
}

// Undelivered returns the submissions the run accepted but never appended,
// because it ended at a limit, an error, a stop tool, or cancellation. It is
// empty until the stream is done. Hosts can resubmit them to a new run.
func (s *EventStream) Undelivered() []Submission {
	select {
	case <-s.done:
	default:
		return nil
	}
	if s.inbox == nil {
		return nil
	}
	s.inbox.mu.Lock()
	defer s.inbox.mu.Unlock()
	return append([]Submission(nil), s.inbox.pending...)
}

func validateSubmission(msg types.UserMessage, mode SubmitMode) error {
	if mode < SubmitQueue || mode > SubmitSide {
		return fmt.Errorf("%w: unknown mode %d", ErrInvalidSubmission, int(mode))
	}
	if len(msg.Parts) == 0 {
		return fmt.Errorf("%w: empty message", ErrInvalidSubmission)
	}
	for _, c := range msg.Parts {
		if _, ok := c.(types.ToolResultPart); ok {
			// A tool result outside the turn that requested it would break
			// tool_use and tool_result pairing.
			return fmt.Errorf("%w: tool results cannot be submitted", ErrInvalidSubmission)
		}
	}
	return nil
}

func newSubmissionID() SubmissionID {
	return SubmissionID("sub_" + types.NewID())
}

// beginTurn registers cancel for the provider call about to start. An
// interrupting submission that arrived after the last safe point cancels it
// at once, so the call does not run in full first.
func (in *inbox) beginTurn(cancel context.CancelCauseFunc) {
	if in == nil {
		return
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	in.cancelTurn = cancel
	in.interruptedBy = ""
	for _, sub := range in.pending {
		if sub.Mode == SubmitInterruptReplace {
			in.interruptedBy = sub.ID
			cancel(errInterruptRequested)
			return
		}
	}
}

// endTurn clears the provider call registered by beginTurn and returns the
// submission that stopped it, if any.
func (in *inbox) endTurn() SubmissionID {
	if in == nil {
		return ""
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	in.cancelTurn = nil
	by := in.interruptedBy
	in.interruptedBy = ""
	return by
}

// take removes and returns the pending submissions a safe point may append.
// Queued messages are only taken when finishing is true. When finishing and
// nothing is pending, the inbox closes, so a later Submit fails with
// ErrRunFinished instead of being lost.
func (in *inbox) take(finishing bool) []Submission {
	if in == nil {
		return nil
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if finishing {
		taken := in.pending
		in.pending = nil
		if len(taken) == 0 {
			in.closed = true
		}
		return taken
	}
	var taken, kept []Submission
	for _, sub := range in.pending {
		if sub.Mode == SubmitQueue {
			kept = append(kept, sub)
		} else {
			taken = append(taken, sub)
		}
	}
	in.pending = kept
	return taken
}

// takeOpen removes and returns every pending submission without closing
// the inbox, for a safe point that resumes the run whatever it takes.
func (in *inbox) takeOpen() []Submission {
	if in == nil {
		return nil
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	taken := in.pending
	in.pending = nil
	return taken
}

// close stops the inbox from accepting submissions.
func (in *inbox) close() {
	if in == nil {
		return
	}
	in.mu.Lock()
	in.closed = true
	in.mu.Unlock()
}

// ── Safe points ─────────────────────────────────────────────────────

// injectSubmissions appends taken submissions to branch, one user node each,
// and reports each with an InjectedDelta. It runs only at safe points, where
// every tool_use on the branch already has its tool_result.
func (a *Agent) injectSubmissions(ctx context.Context, stream *EventStream, tr *tree.Tree, branch types.BranchID, subs []Submission) error {
	stream.flushAcks()
	for _, sub := range subs {
		msg, err := a.admitUserMessage(ctx, stream, tr, branch, sub.Message, sub.Mode.String())
		if err != nil {
			return err
		}
		if sub.Mode == SubmitSteer && treeStoresSteerMarker() {
			msg.Parts = append(append([]types.UserPart(nil), msg.Parts...), types.SteerPart{ID: string(sub.ID)})
		}
		node, err := a.appendNode(ctx, tr, branch, msg)
		if err != nil {
			return err
		}
		stream.send(types.InjectedDelta{SubmissionID: string(sub.ID), Mode: sub.Mode.String(), NodeID: string(node.ID)})
	}
	return nil
}

// injectAtSafePoint appends pending steering and interrupting messages, then
// the results of background children that finished since the last safe
// point. Queued messages wait for the run to finish. It reports whether an
// interrupting message was appended: that message replaces the stopped turn,
// so the run treats it as a new user turn with fresh step limits.
func (a *Agent) injectAtSafePoint(ctx context.Context, stream *EventStream, tr *tree.Tree, branch types.BranchID) (bool, error) {
	subs := stream.inbox.take(false)
	interrupt := false
	for _, sub := range subs {
		if sub.Mode == SubmitInterruptReplace {
			interrupt = true
		}
	}
	if err := a.injectSubmissions(ctx, stream, tr, branch, subs); err != nil {
		return interrupt, err
	}
	_, err := a.injectSpawned(ctx, stream, tr, branch)
	return interrupt, err
}

// reportStarted sends QueuedDelta and InjectedDelta for the submission that
// started the run, once its message is the tip of branch. A host then
// matches every ID Agent.Submit returns the same way.
func (a *Agent) reportStarted(stream *EventStream, tr *tree.Tree, branch types.BranchID) error {
	sub := stream.started
	if sub == nil {
		return nil
	}
	tip, err := tr.Tip(branch)
	if err != nil {
		return err
	}
	stream.send(types.QueuedDelta{SubmissionID: string(sub.ID), Mode: sub.Mode.String(), Position: 1})
	stream.send(types.InjectedDelta{SubmissionID: string(sub.ID), Mode: sub.Mode.String(), NodeID: string(tip.ID)})
	return nil
}

// resumeAtFinish runs where the run would end naturally. It appends every
// pending submission and reports whether the run should continue with them.
// With nothing pending the inbox closes and the run ends.
//
// A run with background children does not finish while one is running or
// its result has not been delivered. It waits for the next result and
// resumes with it instead.
func (a *Agent) resumeAtFinish(ctx context.Context, stream *EventStream, tr *tree.Tree, branch types.BranchID) (bool, error) {
	for stream.spawns.outstanding() {
		resumed, err := a.awaitSpawnedAtFinish(ctx, stream, tr, branch)
		if resumed || err != nil {
			return resumed, err
		}
	}
	subs := stream.inbox.take(true)
	if len(subs) == 0 {
		return false, nil
	}
	return true, a.injectSubmissions(ctx, stream, tr, branch, subs)
}

// commitInterrupted records a provider call stopped by
// SubmitInterruptReplace. The completed text is committed with a
// TruncationPart marker; open and completed tool calls are dropped, so no
// tool_use is left without a result. The replacing message is appended at
// the next safe point.
func (a *Agent) commitInterrupted(ctx context.Context, stream *EventStream, tr *tree.Tree, branch types.BranchID, partial *types.AssistantMessage, by SubmissionID) error {
	stream.send(types.InterruptedDelta{Reason: truncationInterrupted, SubmissionID: string(by)})
	nodeID := ""
	if partial != nil {
		committed := withoutToolCalls(*partial)
		committed.Parts = withoutTruncationMarker(committed.Parts)
		if len(committed.Parts) > 0 {
			committed = withTruncationMarker(committed, truncationInterrupted)
			node, err := a.appendNode(ctx, tr, branch, committed)
			if err != nil {
				return err
			}
			nodeID = string(node.ID)
		}
	}
	stream.send(types.TruncatedDelta{NodeID: nodeID, Reason: truncationInterrupted})
	return nil
}

// truncationInterrupted is the TruncationPart reason for a stopped call.
const truncationInterrupted = "interrupted"

// interruptedPartial builds the partial turn a stopped provider call
// returns: completed text only, marked as truncated. The marker lets a
// durable runner recognize the step as finished rather than uncertain.
func interruptedPartial(agg *DefaultAggregator) *types.AssistantMessage {
	msg, _, dropped := agg.FlushDropped()
	m, _ := msg.(types.AssistantMessage)
	m = withoutToolCalls(m)
	if len(m.Parts) == 0 {
		return nil
	}
	m.Parts = append(m.Parts, types.TruncationPart{Reason: truncationInterrupted, Dropped: dropped})
	return &m
}

// interruptedTurn reports whether msg is the partial turn of a stopped
// provider call.
func interruptedTurn(msg *types.AssistantMessage) bool {
	if msg == nil {
		return false
	}
	for _, c := range msg.Parts {
		if tc, ok := c.(types.TruncationPart); ok && tc.Reason == truncationInterrupted {
			return true
		}
	}
	return false
}

// withTruncationMarker adds a TruncationPart marker to msg when the
// tree's encoding can store it, so a stored conversation always reloads.
func withTruncationMarker(msg types.AssistantMessage, reason string) types.AssistantMessage {
	if !treeStoresTruncationMarker() {
		return msg
	}
	msg.Parts = append(append([]types.AssistantPart(nil), msg.Parts...), types.TruncationPart{Reason: reason})
	return msg
}

func withoutTruncationMarker(content []types.AssistantPart) []types.AssistantPart {
	out := make([]types.AssistantPart, 0, len(content))
	for _, c := range content {
		if _, ok := c.(types.TruncationPart); !ok {
			out = append(out, c)
		}
	}
	return out
}

// treeStoresTruncationMarker and treeStoresSteerMarker report whether the
// tree's message encoding round-trips the marker. A marker it cannot store
// would make the stored conversation fail to reload, so it is left out.
var (
	treeStoresTruncationMarker = sync.OnceValue(func() bool {
		return treeRoundTrips(types.AssistantMessage{Parts: []types.AssistantPart{
			types.TextPart{Text: "x"}, types.TruncationPart{Reason: truncationInterrupted},
		}})
	})
	treeStoresSteerMarker = sync.OnceValue(func() bool {
		return treeRoundTrips(types.UserMessage{Parts: []types.UserPart{
			types.TextPart{Text: "x"}, types.SteerPart{ID: "probe"},
		}})
	})
)

func treeRoundTrips(msg types.Message) bool {
	raw, err := tree.MarshalMessage(msg)
	if err != nil {
		return false
	}
	back, err := tree.UnmarshalMessage(msg.Role(), raw)
	return err == nil && reflect.DeepEqual(back, msg)
}

// ── Continue ────────────────────────────────────────────────────────

// WithAutoContinue lets a run resume up to n times when the output token
// limit cuts a text-only turn short. A truncated turn with a tool call still
// fails with ResponseTruncatedError; the cut-off call never runs.
func WithAutoContinue(n int) AgentOption {
	return func(c *AgentConfig) { c.AutoContinue = n }
}

// Continue resumes the last assistant turn on branch, typically one cut short
// by a stop request or the output token limit. A provider that declares
// types.CapAssistantPrefill continues the partial turn directly; otherwise a
// DefaultContinuePrompt user message asks for the rest. An empty branch means
// the active branch.
//
// It returns ErrNothingToContinue when the branch does not end with an
// assistant turn, and ErrRunActive when a run already holds the branch.
func (a *Agent) Continue(ctx context.Context, branch types.BranchID) (*EventStream, error) {
	if branch == "" {
		branch = a.cfg.Tree.Active()
	}
	tip, err := a.cfg.Tree.Tip(branch)
	if err != nil {
		return nil, err
	}
	last, ok := tip.Message.(types.AssistantMessage)
	if !ok {
		return nil, ErrNothingToContinue
	}
	if len(assistantToolCalls(&last)) > 0 {
		return nil, fmt.Errorf("%w: the last turn requested tools", ErrNothingToContinue)
	}
	var input []types.Message
	if !supportsPrefill(a.cfg.Provider) {
		input = []types.Message{types.UserMsg(types.Text(DefaultContinuePrompt))}
	}
	return a.start(ctx, input, branch)
}

// supportsPrefill reports whether provider resumes a request that ends with
// an assistant turn.
func supportsPrefill(provider types.Provider) bool {
	caps, known := types.ProviderCapabilities(provider)
	return known && caps.Supports(types.CapAssistantPrefill)
}

// appendContinuation prepares the branch for an automatic continuation: a
// prefill-capable provider continues the committed partial turn as is, and
// any other gets the continue prompt.
func (a *Agent) appendContinuation(ctx context.Context, tr *tree.Tree, branch types.BranchID, provider types.Provider) error {
	if supportsPrefill(provider) {
		return nil
	}
	return a.appendToBranch(ctx, tr, branch, types.UserMsg(types.Text(DefaultContinuePrompt)))
}

// ── Agent.Submit ────────────────────────────────────────────────────

// Submit sends msg to branch. When a run is active there, the message joins
// it as mode describes and the run's stream is returned. Otherwise a new run
// starts with msg as its input; its stream reports the returned ID with
// QueuedDelta and InjectedDelta as for a joined run. SubmitSide always starts a separate run on a
// new branch taken from the last safe point of branch. An empty branch means
// the active branch.
//
// A branch held by a run with no consumer (RunDurable) or by a session load
// returns ErrRunActive.
func (a *Agent) Submit(ctx context.Context, branch types.BranchID, msg types.UserMessage, mode SubmitMode) (*EventStream, SubmissionID, error) {
	if err := validateSubmission(msg, mode); err != nil {
		return nil, "", err
	}
	tr := a.cfg.Tree
	if branch == "" {
		branch = tr.Active()
	}
	if mode == SubmitSide {
		return a.submitSide(ctx, branch, msg)
	}
	for {
		sub := &Submission{ID: newSubmissionID(), Mode: mode, Message: msg}
		stream, err := a.startSubmission(ctx, []types.Message{msg}, branch, sub)
		if err == nil {
			return stream, sub.ID, nil
		}
		if !errors.Is(err, ErrRunActive) {
			return nil, "", err
		}
		active, held := activeRuns.holder(tr, branch)
		if !held {
			// The run released the branch after the claim failed; claim it
			// again.
			continue
		}
		if active == nil {
			return nil, "", err
		}
		id, err := active.Submit(msg, mode)
		if err == nil {
			return active, id, nil
		}
		if !errors.Is(err, ErrRunFinished) {
			return nil, "", err
		}
		// The run is past its last safe point; its claim is released before
		// it reports completion, so wait for that and start a new run.
		select {
		case <-active.released:
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	}
}

// submitSide branches from the last safe point of branch and runs msg there.
func (a *Agent) submitSide(ctx context.Context, branch types.BranchID, msg types.UserMessage) (*EventStream, SubmissionID, error) {
	tr := a.cfg.Tree
	from, err := safeTip(tr, branch)
	if err != nil {
		return nil, "", err
	}
	side, node, err := tr.Branch(ctx, from, "side", msg)
	if err != nil {
		return nil, "", err
	}
	a.persistNode(ctx, node)
	sub := &Submission{ID: newSubmissionID(), Mode: SubmitSide, Message: msg}
	stream, err := a.startSubmission(ctx, nil, side, sub)
	if err != nil {
		return nil, "", err
	}
	return stream, sub.ID, nil
}

// safeTip returns the newest node of branch at which the transcript is
// complete: the tip, or the tip's parent when the tip is an assistant turn
// whose tool calls have no results yet.
func safeTip(tr *tree.Tree, branch types.BranchID) (types.NodeID, error) {
	tip, err := tr.Tip(branch)
	if err != nil {
		return "", err
	}
	if am, ok := tip.Message.(types.AssistantMessage); ok && len(assistantToolCalls(&am)) > 0 && tip.ParentID != "" {
		return tip.ParentID, nil
	}
	return tip.ID, nil
}
