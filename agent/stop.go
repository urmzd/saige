package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
)

// StopRequest says why a run is being stopped and, optionally, who asked.
type StopRequest struct {
	// Reason is a short sentence such as "Stopped by a user". It is recorded
	// in the conversation and read by the model on the next turn, so write
	// it in the language the conversation uses. Empty means
	// DefaultStopReason.
	Reason string
	// By names who asked, as the host authenticated them, for example a
	// user ID. It is recorded with the stop and never sent to the model.
	By string
}

// DefaultStopReason is the reason recorded for a StopRequest without one.
const DefaultStopReason = "Stopped on request"

// ErrStopUnsupported means the stream cannot wrap a run up on request: it
// replays stored history, runs elsewhere, or belongs to a run under a
// durable step runner. Cancel still ends such a run.
var ErrStopUnsupported = errors.New("stream does not support stop")

// errStopRequested is the cancellation cause of a run stopped through
// EventStream.Stop. It matches errInterruptRequested, so a provider call in
// flight returns its partial turn and settles its usage as an interrupted
// call does.
var errStopRequested = fmt.Errorf("stop requested: %w", errInterruptRequested)

// truncationStopped is the TruncationPart reason for the partial turn of a
// stopped run.
const truncationStopped = "stopped"

// stopState is the stop request of one run. A request is only acted on once
// the run's input is on the branch (armed), so a stop never loses the
// message that started the turn.
type stopState struct {
	mu sync.Mutex
	// supported is set for runs that can wrap up on request.
	supported bool
	// req is the first accepted request.
	req *StopRequest
	// cancel cancels the run's work with errStopRequested as the cause.
	cancel context.CancelCauseFunc
	armed  bool
	// sealed is set when the run has ended; a later Stop is refused.
	sealed bool
	// stopped is set when the run ended by wrapping up for req.
	stopped bool
}

// Stop ends the run early and wraps the turn up instead of failing it. The
// provider call in flight is cancelled and its completed text is kept as the
// turn's assistant message. Tool calls that have not started do not start,
// running tools are cancelled through their context, and every call the stop
// cut short gets an error result that says so, so no tool call is left
// without a result. A user message that records the stop and its reason is
// appended, tagged with types.StopPart, so the next turn continues with that
// context.
//
// A stopped run reports types.StoppedDelta, then DoneDelta. Wait returns
// nil, and Stopped reports the request. Stop is safe from any goroutine,
// including the one reading Deltas, and never waits for the run. The first
// request wins: a later call returns nil and changes nothing. After the run
// has ended it returns ErrRunFinished. A run that finishes its turn before
// it observes the request ends normally, so read the outcome from Stopped or
// StoppedDelta, not from Stop's return value.
//
// It returns ErrStopUnsupported for replayed and remote streams and for runs
// under a durable step runner, which Cancel still ends.
func (s *EventStream) Stop(req StopRequest) error {
	st := &s.stop
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.supported {
		return ErrStopUnsupported
	}
	if st.req != nil {
		return nil
	}
	if st.sealed {
		return ErrRunFinished
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if req.Reason == "" {
		req.Reason = DefaultStopReason
	}
	st.req = &req
	if st.armed {
		st.cancel(errStopRequested)
	}
	return nil
}

// Stopped reports whether the run ended because of a Stop request, and
// returns that request. It is false while the run is active, and for a run
// that completed, failed, or was cancelled.
func (s *EventStream) Stopped() (StopRequest, bool) {
	st := &s.stop
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.stopped || st.req == nil {
		return StopRequest{}, false
	}
	return *st.req, true
}

// enableStop gives a stoppable run the function that cancels its work. It
// runs before armStop, on the run's goroutine.
func (s *EventStream) enableStop(cancel context.CancelCauseFunc) {
	st := &s.stop
	st.mu.Lock()
	defer st.mu.Unlock()
	st.cancel = cancel
}

// armStop lets a stop request take effect from now on. A request made
// earlier is acted on at once.
func (s *EventStream) armStop() {
	st := &s.stop
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.supported || st.armed {
		return
	}
	st.armed = true
	if st.req != nil {
		st.cancel(errStopRequested)
	}
}

// sealStop closes the run to further stop requests.
func (s *EventStream) sealStop() {
	st := &s.stop
	st.mu.Lock()
	st.sealed = true
	st.mu.Unlock()
}

// stopping returns the request a run is being stopped for, once the request
// has taken effect.
func (s *EventStream) stopping() (StopRequest, bool) {
	st := &s.stop
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.req == nil || !st.armed {
		return StopRequest{}, false
	}
	return *st.req, true
}

// markStopped records that the run ended by wrapping up for req.
func (s *EventStream) markStopped(req StopRequest) {
	st := &s.stop
	st.mu.Lock()
	st.req, st.stopped = &req, true
	st.mu.Unlock()
}

// stopRequested reports whether ctx was cancelled by EventStream.Stop. A
// run whose caller cancelled it first is a cancelled run, not a stopped one.
func stopRequested(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errStopRequested)
}

// stoppedToolText is the error result of a tool call a stop cut short.
func stoppedToolText(req StopRequest) string {
	return "stopped before this call finished: " + req.Reason
}

// stoppedToolResult replaces res, the failed result of a call that ended
// after a stop took effect, with the result that says the stop cut it
// short. A call that succeeded keeps its result: its effects are real. A
// call that failed before the stop is never passed here.
func (s *EventStream) stoppedToolResult(res toolResult) toolResult {
	if res.err == "" {
		return res
	}
	req, ok := s.stopping()
	if !ok {
		return res
	}
	return toolResult{toolCallID: res.toolCallID, err: stoppedToolText(req), approvals: res.approvals, subAgent: res.subAgent}
}

// stopRecord is the user message that records a stopped turn. Its text is
// what the model reads; the StopPart tag is data for the host.
func stopRecord(req StopRequest) types.UserMessage {
	note := "<turn_stopped>\nThe turn above was stopped before it finished, so its answer and tool results may be incomplete. Reason: " +
		req.Reason + "\n</turn_stopped>"
	return types.UserMsg(types.Text(note), types.StopPart{Reason: req.Reason, By: req.By})
}

// stopAfterCall is the stop handling every getAssistantMessage call site
// runs on the call's result, before it looks at the call's error: the turn
// of the loop, the schema-constrained follow-up, and the forced final
// answer. When Stop cancelled the call, or arrived as it returned, the
// turn is committed as stopped and stopped is true; err is then the error
// the caller returns to end the loop. Otherwise it does nothing.
func (a *Agent) stopAfterCall(ctx context.Context, stream *EventStream, tr *tree.Tree, branch types.BranchID, provider types.Provider, msg *types.AssistantMessage, usage *types.UsageDelta, stepName string) (stopped bool, err error) {
	if !stopRequested(ctx) {
		return false, nil
	}
	return true, a.commitStopped(ctx, stream, tr, branch, provider, msg, usage, stepName)
}

// commitStopped records the model turn a stop cut short and returns
// errStopRequested, which ends the loop. The call's usage is reported like
// any turn's. Completed text is committed with a TruncationPart marker
// after it passes the output guardrails; the turn's tool calls are dropped,
// since none of them ran. A turn without text commits nothing, so the branch
// never holds an empty assistant message.
//
// Text the guardrails block, or could not check, is not committed. The
// TruncatedDelta then names no node, which tells a consumer that the text
// it saw streaming was not kept.
func (a *Agent) commitStopped(ctx context.Context, stream *EventStream, tr *tree.Tree, branch types.BranchID, provider types.Provider, partial *types.AssistantMessage, usage *types.UsageDelta, stepName string) error {
	// The run's context is cancelled; the wrap-up writes must still land.
	wctx := context.WithoutCancel(ctx)
	if usage != nil {
		if err := a.reportUsage(ctx, stream, provider, usage); err != nil {
			a.cfg.Logger.Debug("usage of a stopped call not charged", "agent", a.cfg.Name, "error", err)
		}
	}
	nodeID := ""
	if partial != nil {
		committed := withoutToolCalls(*partial)
		committed.Parts = withoutTruncationMarker(committed.Parts)
		if strings.TrimSpace(contentText(committed.Parts)) != "" {
			if err := a.guardOutput(wctx, stream, tr, branch, &committed, stepName); err != nil {
				a.cfg.Logger.Warn("partial text of a stopped turn not kept", "agent", a.cfg.Name, "error", err)
			} else {
				node, err := a.appendNode(wctx, tr, branch, withTruncationMarker(committed, truncationStopped))
				if err != nil {
					return err
				}
				nodeID = string(node.ID)
			}
		}
	}
	stream.send(types.TruncatedDelta{NodeID: nodeID, Reason: truncationStopped})
	return errStopRequested
}

// wrapUpStopped turns the end of a run that Stop cancelled into a clean
// finish. Whatever the loop returned, the branch is made a valid transcript:
// tool calls still without a result get one, and the stop record is
// appended. The run then reports StoppedDelta and no error. A run that was
// not stopped, or that finished before it observed the request, keeps
// runErr.
func (a *Agent) wrapUpStopped(ctx context.Context, stream *EventStream, runErr error) error {
	if runErr == nil || !stopRequested(ctx) {
		return runErr
	}
	req, ok := stream.stopping()
	if !ok {
		return runErr
	}
	if !errors.Is(runErr, errStopRequested) && !errors.Is(runErr, context.Canceled) && !errors.Is(runErr, types.ErrStreamCanceled) {
		a.cfg.Logger.Debug("run error replaced by the stop that caused it", "agent", a.cfg.Name, "error", runErr)
	}
	wctx := context.WithoutCancel(ctx)
	a.answerOpenToolCalls(wctx, stream.branch, stoppedToolText(req))
	node, err := a.appendNode(wctx, a.cfg.Tree, stream.branch, stopRecord(req))
	if err != nil {
		return fmt.Errorf("record stop: %w", err)
	}
	stream.markStopped(req)
	a.cfg.Logger.Info("run stopped on request", "agent", a.cfg.Name, "branch", stream.branch, "reason", req.Reason, "by", req.By)
	stream.send(types.StoppedDelta{Reason: req.Reason, By: req.By, NodeID: string(node.ID)})
	return nil
}
