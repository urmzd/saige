package local

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// expiredReplyKey is the idempotency key recorded when an interrupt expires
// under the deny policy, so replay sees the same denial.
const expiredReplyKey = "expired"

var _ types.InterruptRouter = (*runner)(nil)

// truncatedLLM reports whether result is a provider turn committed before it
// finished, marked with TruncationContent. A turn that still holds tool calls
// does not qualify: their arguments may be incomplete, so the agent must drop
// them before it returns the partial turn. Committing such a turn would let a
// replay run calls the first attempt never ran.
func truncatedLLM(result types.StepResult) bool {
	if result.Kind != types.StepKindLLM || result.Message == nil {
		return false
	}
	marked := false
	for _, c := range result.Message.Content {
		switch c.(type) {
		case types.TruncationContent:
			marked = true
		case types.ToolUseContent:
			return false
		}
	}
	return marked
}

// interruptIdentity is the part of an interrupt that replay must reproduce.
// Timestamps are excluded because a replayed run creates them again.
func interruptIdentity(in types.Interrupt) ([]byte, error) {
	in.CreatedAt = time.Time{}
	in.ExpiresAt = time.Time{}
	return json.Marshal(in)
}

// compactJSON returns a detached, compact copy of raw. The state file is
// indented, which reformats embedded JSON; callers get it back as posted.
func compactJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return append(json.RawMessage(nil), raw...)
	}
	return b.Bytes()
}

// replyFor builds the reply a decided interrupt delivers.
func replyFor(id string, p Interrupt) types.InterruptReply {
	reply := types.InterruptReply{ID: id, IdempotencyKey: p.DecisionKey, Answer: compactJSON(p.Answer)}
	if p.Decision != nil {
		reply.Decision = cloneDecision(*p.Decision)
	}
	return reply
}

// cloneDecision detaches a decision from stored state.
func cloneDecision(d types.ApprovalDecision) types.ApprovalDecision {
	raw, err := json.Marshal(d)
	if err != nil {
		return d
	}
	var out types.ApprovalDecision
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&out) != nil {
		return d
	}
	return out
}

func delivered(reply types.InterruptReply) <-chan types.InterruptReply {
	ch := make(chan types.InterruptReply, 1)
	ch <- reply
	close(ch)
	return ch
}

// post records in on s. It returns the stored reply when the interrupt is
// already decided, types.ErrSuspended when it is new or still pending, and
// changed reports whether s must be saved.
func post(s *State, in types.Interrupt, ttl time.Duration) (reply *types.InterruptReply, changed bool, err error) {
	if in.ID == "" {
		return nil, false, errors.New("interrupt ID required")
	}
	if in.RunID == "" {
		in.RunID = s.RunID
	}
	identity, err := interruptIdentity(in)
	if err != nil {
		return nil, false, err
	}
	now := time.Now().UTC()
	if p, ok := s.Interrupts[in.ID]; ok {
		if p.Posted == nil {
			return nil, false, fmt.Errorf("%w: interrupt %s is an approval request", ErrConflict, in.ID)
		}
		old, err := interruptIdentity(*p.Posted)
		if err != nil {
			return nil, false, err
		}
		if !bytes.Equal(identity, old) {
			return nil, false, fmt.Errorf("%w: interrupt %s changed on replay", ErrConflict, in.ID)
		}
		if p.Decision != nil {
			r := replyFor(in.ID, p)
			return &r, false, nil
		}
		if p.Posted.Expired(now) {
			return expire(s, in.ID, p, now)
		}
		return nil, false, types.ErrSuspended
	}
	stored := in
	stored.Payload = append(json.RawMessage(nil), in.Payload...)
	stored.Markers = append([]types.Marker(nil), in.Markers...)
	stored.Path = append([]string(nil), in.Path...)
	if stored.CreatedAt.IsZero() {
		stored.CreatedAt = now
	}
	if stored.ExpiresAt.IsZero() {
		stored.ExpiresAt = stored.CreatedAt.Add(ttl)
	}
	s.Interrupts[in.ID] = Interrupt{Posted: &stored, CreatedAt: stored.CreatedAt, ExpiresAt: stored.ExpiresAt}
	s.History = append(s.History, Event{Sequence: len(s.History) + 1, At: now, Kind: "interrupt.posted", ID: in.ID})
	return nil, true, types.ErrSuspended
}

// expire applies the interrupt's expiry policy. A denial is recorded so every
// later replay sees the same outcome.
func expire(s *State, id string, p Interrupt, now time.Time) (*types.InterruptReply, bool, error) {
	switch p.Posted.Policy.OnExpire {
	case "", types.InterruptExpireDeny:
		decision := types.ApprovalDecision{Approved: false, Message: "interrupt expired without a reply"}
		p.Decision = &decision
		p.DecisionKey = expiredReplyKey
		p.DecidedAt = now
		s.Interrupts[id] = p
		s.History = append(s.History, Event{Sequence: len(s.History) + 1, At: now, Kind: "interrupt.expired", ID: id})
		r := replyFor(id, p)
		return &r, true, nil
	default:
		// The local engine has no parent run to escalate to, so escalation
		// fails the run like the fail policy and leaves the choice to the host.
		return nil, false, fmt.Errorf("%w: %s (policy %s)", types.ErrInterruptExpired, id, p.Posted.Policy.OnExpire)
	}
}

// pending lists unanswered, unexpired interrupts in s for runID, oldest first.
// An empty runID or the engine run ID selects every interrupt in the run,
// including those raised by delegated children.
func pending(s *State, runID string) []types.Interrupt {
	now := time.Now().UTC()
	var out []types.Interrupt
	for id, p := range s.Interrupts {
		if p.Decision != nil || !now.Before(p.ExpiresAt) {
			continue
		}
		in := asInterrupt(s.RunID, id, p)
		if runID != "" && runID != s.RunID && runID != in.RunID {
			continue
		}
		out = append(out, in)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// asInterrupt presents a stored record as a types.Interrupt. Approvals raised
// through ResolveApproval carry the tool call as their payload.
func asInterrupt(runID, id string, p Interrupt) types.Interrupt {
	if p.Posted != nil {
		in := *p.Posted
		in.Payload = compactJSON(in.Payload)
		in.Markers = append([]types.Marker(nil), in.Markers...)
		in.Path = append([]string(nil), in.Path...)
		return in
	}
	payload, _ := json.Marshal(p.Request.ToolCall)
	return types.Interrupt{
		ID:        id,
		RunID:     runID,
		Kind:      types.InterruptApproval,
		Payload:   payload,
		Markers:   append([]types.Marker(nil), p.Request.Markers...),
		CreatedAt: p.CreatedAt,
		ExpiresAt: p.ExpiresAt,
	}
}

// Post records an interrupt raised inside a running worker. A new or still
// pending interrupt returns types.ErrSuspended: the run stops, and a later Run
// after Reply or Decide delivers the stored reply on the returned channel. An
// interrupt posted without ExpiresAt expires after the engine's ApprovalTTL.
// Replay must post the same interrupt; a changed one returns ErrConflict.
func (r *runner) Post(ctx context.Context, in types.Interrupt) (<-chan types.InterruptReply, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.poisoned != nil {
		return nil, r.poisoned
	}
	reply, changed, err := post(&r.state, in, r.ttl)
	if changed {
		if saveErr := r.save(); saveErr != nil {
			return nil, saveErr
		}
	}
	if err != nil {
		return nil, err
	}
	return delivered(*reply), nil
}

// Reply records a reply from inside the worker that holds the run.
func (r *runner) Reply(ctx context.Context, reply types.InterruptReply) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.poisoned != nil {
		return r.poisoned
	}
	if err := applyReply(&r.state, reply); err != nil {
		return err
	}
	if err := r.save(); err != nil {
		return err
	}
	return signal(ctx, r.notifier, Signal{RunID: r.state.RunID, Kind: SignalReply, ID: reply.ID})
}

// Pending lists the run's unanswered interrupts.
func (r *runner) Pending(ctx context.Context, runID string) ([]types.Interrupt, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return pending(&r.state, runID), nil
}

// Router is the host-side types.InterruptRouter for runs of one engine. It
// changes run state under the worker lease, so it returns ErrBusy while a
// worker runs; a durable run releases its worker as soon as it suspends.
type Router struct {
	Engine *Engine
	// RunID limits Reply to one run. When empty, Reply searches every run
	// and fails with ErrConflict if more than one holds the interrupt ID.
	RunID string
}

var _ types.InterruptRouter = (*Router)(nil)

// Router returns a host-side interrupt router for the engine's runs.
func (e *Engine) Router() *Router { return &Router{Engine: e} }

// Post records an interrupt on the run named by in.RunID from outside a
// worker. It returns types.ErrSuspended until a reply arrives, and the stored
// reply once one has.
func (rt *Router) Post(ctx context.Context, in types.Interrupt) (<-chan types.InterruptReply, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if in.RunID == "" {
		return nil, errors.New("interrupt run ID required")
	}
	s, err := rt.Engine.Inspect(in.RunID)
	if err != nil {
		return nil, err
	}
	ttl := rt.Engine.ApprovalTTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	var reply *types.InterruptReply
	var postErr error
	err = rt.Engine.update(in.RunID, s.Revision, func(s *State) error {
		if s.Status == statusCompleted || s.Status == statusCancelled {
			return ErrClosed
		}
		reply, _, postErr = post(s, in, ttl)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if postErr != nil {
		return nil, postErr
	}
	return delivered(*reply), nil
}

// Reply records a decision or answer. It is idempotent by (ID,
// IdempotencyKey): the same reply again is a no-op, and a different reply to
// a decided interrupt returns ErrConflict. An unknown ID matches
// types.ErrInterruptNotFound and a late reply matches
// types.ErrInterruptExpired. The next Run of the owning run picks it up.
func (rt *Router) Reply(ctx context.Context, reply types.InterruptReply) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if reply.ID == "" {
		return fmt.Errorf("%w: empty interrupt ID", types.ErrInterruptNotFound)
	}
	owner, err := rt.owner(reply.ID)
	if err != nil {
		return err
	}
	if err := rt.Engine.update(owner.RunID, owner.Revision, func(s *State) error { return applyReply(s, reply) }); err != nil {
		return err
	}
	return signal(ctx, rt.Engine.Notifier, Signal{RunID: owner.RunID, Kind: SignalReply, ID: reply.ID})
}

// owner finds the run holding interrupt id.
func (rt *Router) owner(id string) (State, error) {
	if rt.RunID != "" {
		s, err := rt.Engine.Inspect(rt.RunID)
		if err != nil {
			return State{}, err
		}
		if _, ok := s.Interrupts[id]; !ok {
			return State{}, fmt.Errorf("%w: %s", types.ErrInterruptNotFound, id)
		}
		return s, nil
	}
	runs, err := rt.Engine.List(func(s State) bool { _, ok := s.Interrupts[id]; return ok })
	if err != nil {
		return State{}, err
	}
	switch len(runs) {
	case 0:
		return State{}, fmt.Errorf("%w: %s", types.ErrInterruptNotFound, id)
	case 1:
		return runs[0], nil
	default:
		return State{}, fmt.Errorf("%w: interrupt %s exists in %d runs; set Router.RunID", ErrConflict, id, len(runs))
	}
}

// Pending lists unanswered, unexpired interrupts of runID, including those
// raised by delegated children, oldest first. An unknown run has none.
func (rt *Router) Pending(ctx context.Context, runID string) ([]types.Interrupt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s, err := rt.Engine.Inspect(runID)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return pending(&s, runID), nil
}
