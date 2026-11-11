package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/urmzd/saige/durable/ledger"
	"github.com/urmzd/saige/durable/run"
	"slices"
	"strings"

	"github.com/urmzd/saige/flow/definition"
	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/session"
	"github.com/urmzd/saige/flow/thread"
)

// WithSessions indexes every accepted conversation and its child runs. Hosts
// pass the same store to the API and workers. Without it legacy runs keep their
// original journal key and recorded inputs remain replayable.
func WithSessions(s session.Store) Option { return func(r *Runtime) { r.sessions = s } }

// AcceptSession starts an admitted run in an existing session. It is a host
// boundary, like Accept: the host checks the person's permission first.
func (r *Runtime) AcceptSession(ctx context.Context, e event.Envelope, agent string, rec session.Record) (Acceptance, error) {
	return r.acceptSession(ctx, e, agent, rec, RunKey(e.Tenant, e.ID, agent))
}

// AcceptOwnSession accepts an event whose ID already identifies one agent's turn.
func (r *Runtime) AcceptOwnSession(ctx context.Context, e event.Envelope, agent string, rec session.Record) (Acceptance, error) {
	return r.acceptSession(ctx, e, agent, rec, "")
}
func (r *Runtime) acceptSession(ctx context.Context, e event.Envelope, agent string, rec session.Record, runID string) (Acceptance, error) {
	if err := e.Validate(); err != nil {
		return Acceptance{}, err
	}
	if event.IsControl(e.Channel) || event.Family(e.Type) == "run" || rec.Key.Tenant != e.Tenant || rec.Key.Agent != definition.NormalizeName(agent) || r.sessions == nil {
		return Acceptance{}, session.ErrDenied
	}
	held, err := r.sessions.Get(ctx, e.Tenant, rec.ID)
	if err != nil {
		return Acceptance{}, err
	}
	if held != rec {
		return Acceptance{}, fmt.Errorf("%w: session identity changed", session.ErrInvalid)
	}
	return r.start(ctx, Input{Trigger: e, Subscriber: agent, Run: runID, Session: &held, RequireBudget: true}, "")
}

func (in Input) threadKey() thread.Key {
	if in.Session != nil {
		return in.Session.ThreadKey()
	}
	return thread.KeyOf(in.Trigger, in.Subscriber)
}
func (r *Runtime) bindSession(ctx context.Context, in *Input) error {
	if in.Session != nil {
		held, err := r.sessions.Get(ctx, in.Trigger.Tenant, in.Session.ID)
		if err != nil {
			return err
		}
		if held.Key != in.Session.Key || held.Key.Agent != definition.NormalizeName(in.Subscriber) {
			return session.ErrDenied
		}
		in.Session = &held
		return nil
	}
	key := thread.KeyOf(in.Trigger, in.Subscriber)
	rec, err := r.sessions.Open(ctx, in.Trigger.Tenant, session.Open{Parent: key.Surface, Agent: key.Agent, Channel: in.Trigger.Channel})
	if err != nil {
		return fmt.Errorf("runtime: open session: %w", err)
	}
	in.Session = &rec
	return nil
}

// childSession applies the pinned parent's call grant, then opens the stable
// child key. The run ID still identifies one call, not the child's conversation.
func (r *Runtime) childSession(ctx context.Context, parent Input, in *Input, label string) error {
	loaded, err := r.defs.Verify(ctx, parent.Trigger.Tenant, parent.Pin)
	if err != nil {
		return err
	}
	if !slices.Contains(loaded.Agent.Spec.Access.Spawn, definition.NormalizeName(in.Subscriber)) {
		return session.ErrDenied
	}
	if r.sessions == nil {
		return nil
	}
	if err := r.bindSession(ctx, &parent); err != nil {
		return err
	}
	maxDepth := loaded.Agent.Policy.SessionDepth
	if maxDepth == 0 {
		maxDepth = 3
	}
	if parent.Session.Depth >= maxDepth {
		return fmt.Errorf("%w: session depth limit %d", session.ErrDenied, maxDepth)
	}
	rec, err := r.sessions.Open(ctx, parent.Trigger.Tenant, session.Open{Parent: parent.Session.ID, Agent: in.Subscriber, Label: label})
	if err != nil {
		return err
	}
	in.Session = &rec
	// Children have their own stream. Surfaces can follow it explicitly without
	// mixing child deltas into the parent's answer.
	in.Trigger.Channel = rec.Channel
	return nil
}

// SessionCall is a durable call request. ID is stable across attempts and must
// be used as the host admission idempotency key. Session is already authorized
// against the parent's pinned call policy and persisted before admission.
type SessionCall struct {
	ID         string
	Agent      string
	Text       string
	Label      string
	Fresh      bool
	Session    session.Record
	QuestionID string
}

// ChildStarter is the host's one admission path. It returns as soon as a child
// is durably accepted, never when that child ends. Runtime owns the wait.
type ChildStarter func(context.Context, Input, SessionCall) (string, error)

func WithChildStarter(start ChildStarter) Option {
	return func(r *Runtime) { r.childStarter = start; r.childRuns = true }
}

// AcceptSessionChild attaches admitted work to the run that requested it. The
// parent's durable wait resumes from the child's terminal ledger result.
func (r *Runtime) AcceptSessionChild(ctx context.Context, e event.Envelope, agent string, rec session.Record, parentRun, questionID string) (Acceptance, error) {
	return r.acceptSessionChild(ctx, e, agent, rec, parentRun, questionID, RunKey(e.Tenant, e.ID, agent))
}

// AcceptOwnSessionChild is AcceptSessionChild for a host-owned, per-agent event ID.
func (r *Runtime) AcceptOwnSessionChild(ctx context.Context, e event.Envelope, agent string, rec session.Record, parentRun, questionID string) (Acceptance, error) {
	return r.acceptSessionChild(ctx, e, agent, rec, parentRun, questionID, "")
}
func (r *Runtime) acceptSessionChild(ctx context.Context, e event.Envelope, agent string, rec session.Record, parentRun, questionID, runID string) (Acceptance, error) {
	if err := e.Validate(); err != nil {
		return Acceptance{}, err
	}
	if r.sessions == nil || rec.Key.Tenant != e.Tenant || rec.Key.Agent != definition.NormalizeName(agent) || event.IsControl(e.Channel) || event.Family(e.Type) == "run" {
		return Acceptance{}, session.ErrDenied
	}
	parent, err := r.loadRun(ctx, parentRun)
	if err != nil {
		return Acceptance{}, err
	}
	var pin Input
	if err := json.Unmarshal(parent.Input, &pin); err != nil {
		return Acceptance{}, err
	}
	if pin.Trigger.Tenant != e.Tenant {
		return Acceptance{}, session.ErrNotFound
	}
	if pin.Session == nil || rec.Key.Parent != pin.Session.ID {
		return Acceptance{}, session.ErrDenied
	}
	return r.start(ctx, Input{Trigger: e, Subscriber: agent, Run: runID, Session: &rec, Parent: &ParentRef{RunID: parentRun, QuestionID: questionID}}, "")
}

func (r *Runtime) reserveChild(ctx context.Context, parent Input, in Input, childRun string) (session.AdmissionStore, error) {
	if r.sessions == nil {
		return nil, nil
	}
	store, ok := r.sessions.(session.AdmissionStore)
	if !ok {
		return nil, fmt.Errorf("%w: session store cannot enforce atomic child admission", session.ErrDenied)
	}
	reservations, err := store.Reservations(ctx, in.Trigger.Tenant, in.Session.Key.Parent)
	if err != nil {
		return nil, err
	}
	for _, reservation := range reservations {
		ended, err := r.reservationEnded(ctx, in.Trigger.Tenant, reservation)
		if err != nil {
			return nil, err
		}
		if ended {
			if err := store.Release(ctx, in.Trigger.Tenant, in.Session.Key.Parent, reservation.Call); err != nil {
				return nil, err
			}
		}
	}
	loaded, err := r.defs.Verify(ctx, parent.Trigger.Tenant, parent.Pin)
	if err != nil {
		return nil, err
	}
	maxChildren := loaded.Agent.Policy.OpenChildren
	if maxChildren == 0 {
		maxChildren = 8
	}
	if err := store.Reserve(ctx, in.Trigger.Tenant, in.Session.Key.Parent, admissionKey(in), in.Session.ID, maxChildren); err != nil {
		return nil, err
	}
	if err := store.BindReservation(context.WithoutCancel(ctx), in.Trigger.Tenant, in.Session.Key.Parent, admissionKey(in), childRun); err != nil {
		return nil, err
	}
	return store, nil
}

// stopSessionChildren follows durable lineage, including calls started with
// wait:false. It never stops another run merely for sharing a session.
func (r *Runtime) stopSessionChildren(ctx context.Context, in Input, body []byte) error {
	if in.Session == nil || r.sessions == nil {
		return nil
	}
	store, ok := r.sessions.(session.AdmissionStore)
	if !ok {
		return nil
	}
	claims, err := store.Reservations(ctx, in.Trigger.Tenant, in.Session.ID)
	if err != nil {
		return err
	}
	var request event.StopRequest
	if err := json.Unmarshal(body, &request); err != nil {
		return err
	}
	for _, claim := range claims {
		id := claim.Run
		if id == "" {
			id = RunID(in.Trigger.Tenant, claim.Call)
		}
		child, err := r.loadRun(ctx, id)
		if err != nil {
			continue
		}
		if child.ParentRunID != in.runID() || child.Status.Terminal() {
			continue
		}
		if err := r.Stop(ctx, StopRequest{Tenant: in.Trigger.Tenant, RunID: id, Reason: request.Reason, By: request.By}); err != nil {
			return err
		}
	}
	return nil
}

// AcceptOwnVersion admits a host-owned event at the recorded definition version.
// Authorization still belongs to the host; the durable input pins this version.
func (r *Runtime) AcceptOwnVersion(ctx context.Context, e event.Envelope, agent, version string) (Acceptance, error) {
	if err := e.Validate(); err != nil {
		return Acceptance{}, err
	}
	if agent == "" || version == "" || event.IsControl(e.Channel) || event.Family(e.Type) == "run" {
		return Acceptance{}, ErrInvalid
	}
	return r.start(ctx, Input{Trigger: e, Subscriber: agent}, version)
}

// WithChildRunID names a governed child's deterministic run before admission.
// It must match ChildStarter's result. Prebinding lets recovery distinguish an
// unstarted call from accepted work even if a process dies in the handoff.
func WithChildRunID(id func(Input, SessionCall) string) Option {
	return func(r *Runtime) { r.childRunID = id }
}

const ownedAdmissionPrefix = "session-call.v1:"

func admissionKey(in Input) string { return ownedAdmissionPrefix + in.Trigger.ID }

// reservationEnded never frees an accepted child from an inference about time.
// The v1 protocol binds the child ID before invoking admission. Only a terminal
// owner can abandon an absent child; a ledger tombstone fences late acceptance.
func (r *Runtime) reservationEnded(ctx context.Context, tenant string, claim session.Reservation) (bool, error) {
	if claim.Run != "" {
		child, err := r.loadRun(ctx, claim.Run)
		if err == nil {
			return child.Status.Terminal(), nil
		}
		if !errors.Is(err, run.ErrNotFound) {
			return false, err
		}
	}
	call, ok := strings.CutPrefix(claim.Call, ownedAdmissionPrefix+"child-")
	if !ok {
		return false, nil
	}
	owner, _, ok := strings.Cut(call, "-")
	if !ok {
		return false, nil
	}
	parent, err := r.loadRun(ctx, owner)
	if errors.Is(err, run.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var in Input
	if json.Unmarshal(parent.Input, &in) != nil || in.Trigger.Tenant != tenant || !parent.Status.Terminal() {
		return false, nil
	}
	if claim.Run == "" {
		return true, nil
	} // Never passed the pre-admission binding fence.
	l, ok := r.stops.(ledger.Ledger)
	if !ok {
		return false, fmt.Errorf("%w: admission repair requires the full durable ledger", session.ErrDenied)
	}
	const abandoned = "fiber.session.abandoned.v1"
	raw, _ := json.Marshal(map[string]string{"owner": owner, "session": claim.Child})
	tombstone := run.Run{ID: claim.Run, Name: abandoned, Input: raw, ParentRunID: owner, Status: run.StatusPending, MaxAttempts: 1, CreatedAt: r.now().UTC()}
	if err := l.Accept(ctx, tombstone); err != nil {
		return false, err
	}
	held, _, err := l.Load(ctx, claim.Run)
	if err != nil {
		return false, err
	}
	if held.Name != abandoned {
		return held.Status.Terminal(), nil
	} // Real admission won.
	if err := l.Complete(ctx, claim.Run, 0, run.Result{Status: run.StatusFailed, Error: "parent ended before child admission"}); err != nil && !errors.Is(err, run.ErrAlreadyTerminal) {
		return false, err
	}
	return true, nil
}
