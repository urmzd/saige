package duraturo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	dt "github.com/urmzd/duraturo"
	"github.com/urmzd/duraturo/pkg/run"

	"github.com/urmzd/saige/agent/types"
)

const (
	interruptPrefix = "interrupt:" // a posted interrupt
	replyPrefix     = "reply:"     // the event that answers it

	// expiredReplyKey is the idempotency key recorded when an interrupt
	// expires under the deny policy, so replay sees the same denial.
	expiredReplyKey = "expired"
)

// postedRecord is the recorded form of a posted interrupt. Request is set for
// approvals raised through ResolveApproval.
type postedRecord struct {
	Interrupt types.Interrupt        `json:"interrupt"`
	Request   *types.ApprovalRequest `json:"request,omitempty"`
}

// replyPayload is the recorded reply event. Decision and Answer are the JSON
// the host supplied.
type replyPayload struct {
	Key      string          `json:"key"`
	Decision json.RawMessage `json:"decision,omitempty"`
	Answer   json.RawMessage `json:"answer,omitempty"`
}

// posted is the outcome of one Post within an attempt.
type posted struct {
	reply types.InterruptReply
	err   error
}

// post records in, then returns its reply or parks the run. Within one
// attempt a repeated post of the same ID returns the first outcome.
func (r *runner) post(ctx context.Context, in types.Interrupt, req *types.ApprovalRequest) (types.InterruptReply, error) {
	if in.ID == "" {
		return types.InterruptReply{}, errors.New("interrupt ID required")
	}
	if err := ctx.Err(); err != nil {
		return types.InterruptReply{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.posted[in.ID]; ok {
		return p.reply, p.err
	}
	reply, err := r.postLocked(ctx, in, req)
	r.posted[in.ID] = posted{reply: reply, err: err}
	return reply, err
}

func (r *runner) postLocked(ctx context.Context, in types.Interrupt, req *types.ApprovalRequest) (types.InterruptReply, error) {
	rec := postedRecord{Interrupt: stamp(in, time.Now().UTC(), r.e.ttl()), Request: req}
	fresh := false
	raw, err := dt.Step(ctx, interruptPrefix+in.ID, func(context.Context) (json.RawMessage, error) {
		fresh = true
		return json.Marshal(rec)
	})
	if err != nil {
		return types.InterruptReply{}, err
	}
	stored, err := decodePosted(raw)
	if err != nil {
		return types.InterruptReply{}, err
	}
	if err := sameRequest(rec, stored); err != nil {
		return types.InterruptReply{}, err
	}
	payload, err := dt.Event[json.RawMessage](ctx, replyPrefix+in.ID)
	if err == nil {
		return replyFrom(in.ID, payload)
	}
	if !errors.Is(err, run.ErrParked) {
		return types.InterruptReply{}, err
	}
	// A new interrupt always reaches the host first, even one posted with a
	// deadline that has passed.
	if fresh || !stored.Interrupt.Expired(time.Now()) {
		return types.InterruptReply{}, fmt.Errorf("%w: %w", types.ErrSuspended, err)
	}
	if stored.Request != nil {
		return types.InterruptReply{}, fmt.Errorf("%w: approval expired", ErrClosed)
	}
	switch stored.Interrupt.Policy.OnExpire {
	case "", types.InterruptExpireDeny:
		// Record the denial where the reply belongs, so every replay sees
		// the same outcome. A host reply that won the race is adopted.
		denial, err := json.Marshal(types.ApprovalDecision{Approved: false, Message: "interrupt expired without a reply"})
		if err != nil {
			return types.InterruptReply{}, err
		}
		out, err := r.e.writeReply(ctx, r.runID, eventKey(scope(ctx), in.ID), in.ID, replyPayload{Key: expiredReplyKey, Decision: denial})
		if err != nil && !errors.Is(err, run.ErrAlreadyRecorded) {
			return types.InterruptReply{}, err
		}
		return replyFrom(in.ID, out.Output)
	default:
		// No parent run exists to escalate to, so escalation fails the run
		// like the fail policy and leaves the choice to the host.
		return types.InterruptReply{}, fmt.Errorf("%w: %s (policy %s)", types.ErrInterruptExpired, in.ID, stored.Interrupt.Policy.OnExpire)
	}
}

// stamp sets the creation time and the default expiry of a new interrupt.
func stamp(in types.Interrupt, now time.Time, ttl time.Duration) types.Interrupt {
	in.Payload = compactJSON(in.Payload)
	in.Markers = append([]types.Marker(nil), in.Markers...)
	in.Path = append([]string(nil), in.Path...)
	if in.CreatedAt.IsZero() {
		in.CreatedAt = now
	}
	if in.ExpiresAt.IsZero() {
		in.ExpiresAt = in.CreatedAt.Add(ttl)
	}
	return in
}

// sameRequest rejects a post that differs from the recorded one. Timestamps
// are excluded because a replayed run creates them again.
func sameRequest(live, stored postedRecord) error {
	if (live.Request == nil) != (stored.Request == nil) {
		return fmt.Errorf("%w: interrupt %s changed kind on replay", ErrConflict, live.Interrupt.ID)
	}
	var a, b []byte
	var err error
	if live.Request != nil {
		if a, err = json.Marshal(live.Request); err == nil {
			b, err = json.Marshal(stored.Request)
		}
	} else {
		if a, err = identity(live.Interrupt); err == nil {
			b, err = identity(stored.Interrupt)
		}
	}
	if err != nil {
		return err
	}
	if !bytes.Equal(a, b) {
		return fmt.Errorf("%w: interrupt %s changed on replay", ErrConflict, live.Interrupt.ID)
	}
	return nil
}

func identity(in types.Interrupt) ([]byte, error) {
	in.CreatedAt = time.Time{}
	in.ExpiresAt = time.Time{}
	in.Payload = compactJSON(in.Payload)
	return json.Marshal(in)
}

// decodePosted keeps large integers in tool arguments exact.
func decodePosted(raw []byte) (postedRecord, error) {
	var p postedRecord
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	err := d.Decode(&p)
	return p, err
}

func replyFrom(id string, raw []byte) (types.InterruptReply, error) {
	var p replyPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return types.InterruptReply{}, fmt.Errorf("decode reply: %w", err)
	}
	reply := types.InterruptReply{ID: id, IdempotencyKey: p.Key, Answer: compactJSON(p.Answer)}
	if len(p.Decision) > 0 {
		d := json.NewDecoder(bytes.NewReader(p.Decision))
		d.UseNumber()
		if err := d.Decode(&reply.Decision); err != nil {
			return types.InterruptReply{}, fmt.Errorf("decode reply: %w", err)
		}
	}
	return reply, nil
}

func delivered(reply types.InterruptReply) <-chan types.InterruptReply {
	ch := make(chan types.InterruptReply, 1)
	ch <- reply
	close(ch)
	return ch
}

// compactJSON returns a detached, compact copy of raw.
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

// scope returns the key prefix of records made at this point of the run:
// empty at the top of the workflow, or the enclosing step's key plus "/".
func scope(ctx context.Context) string {
	info, _ := dt.FromContext(ctx)
	key := strings.TrimPrefix(dt.IdempotencyKey(ctx), info.RunID+":")
	if key == "" {
		return ""
	}
	return key + "/"
}

// eventKey is the record key of the first reply event for interrupt id.
func eventKey(prefix, id string) string {
	return prefix + "event:" + replyPrefix + id + "#0"
}

// writeReply records a reply event. On a conflict it returns the stored
// record with run.ErrAlreadyRecorded.
func (e *Engine) writeReply(ctx context.Context, runID, key, id string, p replyPayload) (run.Record, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return run.Record{}, err
	}
	return e.lgr.Record(ctx, run.Record{
		RunID: runID, Key: key, Kind: run.KindEvent, Name: replyPrefix + id,
		Codec: run.JSONCodec{}.ContentType(), Status: run.RecordOK, Output: raw,
	})
}

// found is one recorded interrupt and the key of its reply event.
type found struct {
	posted   postedRecord
	replyKey string
}

// interrupts indexes the posted interrupts among records by ID.
func interrupts(records []run.Record) (map[string]found, map[string]run.Record, error) {
	byKey := make(map[string]run.Record, len(records))
	for _, rec := range records {
		byKey[rec.Key] = rec
	}
	out := map[string]found{}
	for _, rec := range records {
		if rec.Kind != run.KindStep || !strings.HasPrefix(rec.Name, interruptPrefix) {
			continue
		}
		id := strings.TrimPrefix(rec.Name, interruptPrefix)
		prefix, ok := strings.CutSuffix(rec.Key, rec.Name+"#0")
		if !ok {
			continue
		}
		p, err := decodePosted(rec.Output)
		if err != nil {
			return nil, nil, err
		}
		out[id] = found{posted: p, replyKey: eventKey(prefix, id)}
	}
	return out, byKey, nil
}

// reply records a host reply for an interrupt of run runID and wakes the
// run. It is idempotent by (ID, IdempotencyKey): the same reply again is a
// no-op, and a different reply to an answered interrupt returns ErrConflict.
func (e *Engine) reply(ctx context.Context, runID string, reply types.InterruptReply) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if reply.IdempotencyKey == "" {
		return errors.New("reply idempotency key required")
	}
	r, records, err := e.lgr.Load(ctx, runID)
	if err != nil {
		return err
	}
	if r.Status.Terminal() {
		return ErrClosed
	}
	all, byKey, err := interrupts(records)
	if err != nil {
		return err
	}
	f, ok := all[reply.ID]
	if !ok {
		return fmt.Errorf("%w: %s", types.ErrInterruptNotFound, reply.ID)
	}
	decision, err := json.Marshal(reply.Decision)
	if err != nil {
		return err
	}
	p := replyPayload{Key: reply.IdempotencyKey, Decision: decision, Answer: compactJSON(reply.Answer)}
	stored, exists := byKey[f.replyKey]
	if !exists {
		if f.posted.Interrupt.Expired(time.Now()) {
			return fmt.Errorf("%w: %w", ErrClosed, types.ErrInterruptExpired)
		}
		stored, err = e.writeReply(ctx, runID, f.replyKey, reply.ID, p)
		exists = errors.Is(err, run.ErrAlreadyRecorded)
		if err != nil && !exists {
			return err
		}
	}
	if exists {
		if err := sameReply(stored, p); err != nil {
			return err
		}
	}
	if err := e.q.Enqueue(ctx, runID, 0); err != nil {
		return fmt.Errorf("reply is recorded; enqueue run %s: %w", runID, err)
	}
	return nil
}

// sameReply accepts a retry of the recorded reply and rejects anything else.
func sameReply(stored run.Record, p replyPayload) error {
	var old replyPayload
	if err := json.Unmarshal(stored.Output, &old); err != nil {
		return err
	}
	if old.Key == p.Key && bytes.Equal(compactJSON(old.Decision), compactJSON(p.Decision)) && bytes.Equal(compactJSON(old.Answer), compactJSON(p.Answer)) {
		return nil
	}
	if old.Key == expiredReplyKey && p.Key != expiredReplyKey {
		// The denial was recorded by expiry, not by a host reply.
		return fmt.Errorf("%w: %w", ErrClosed, types.ErrInterruptExpired)
	}
	return ErrConflict
}

// Decide answers approval interruptID of run id. key is the reply's
// idempotency key. The host authenticates the decision maker.
func (e *Engine) Decide(ctx context.Context, id, interruptID, key string, decision types.ApprovalDecision) error {
	return e.reply(ctx, id, types.InterruptReply{ID: interruptID, IdempotencyKey: key, Decision: decision})
}

// Router is the host-side types.InterruptRouter for runs of one engine.
type Router struct {
	Engine *Engine
	// RunID limits Reply to one run. When empty, Reply searches every
	// unfinished run and fails with ErrConflict if more than one holds the
	// interrupt ID.
	RunID string
}

var _ types.InterruptRouter = (*Router)(nil)

// Router returns a host-side interrupt router for the engine's runs.
func (e *Engine) Router() *Router { return &Router{Engine: e} }

// Post records an interrupt on the run named by in.RunID from outside a
// worker. It returns types.ErrSuspended until a reply arrives, and the stored
// reply once one has. The run sees it when it posts the same ID.
func (rt *Router) Post(ctx context.Context, in types.Interrupt) (<-chan types.InterruptReply, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if in.ID == "" || in.RunID == "" {
		return nil, errors.New("interrupt ID and run ID required")
	}
	e := rt.Engine
	r, records, err := e.lgr.Load(ctx, in.RunID)
	if err != nil {
		return nil, err
	}
	if r.Status.Terminal() {
		return nil, ErrClosed
	}
	rec := postedRecord{Interrupt: stamp(in, time.Now().UTC(), e.ttl())}
	all, byKey, err := interrupts(records)
	if err != nil {
		return nil, err
	}
	f, ok := all[in.ID]
	if !ok {
		raw, err := json.Marshal(rec)
		if err != nil {
			return nil, err
		}
		// The record matches the one the run's own Post would make, so the
		// run replays it when it posts the same ID.
		stored, err := e.lgr.Record(ctx, run.Record{
			RunID: in.RunID, Key: interruptPrefix + in.ID + "#0", Kind: run.KindStep, Name: interruptPrefix + in.ID,
			Codec: run.JSONCodec{}.ContentType(), Status: run.RecordOK, Output: raw,
		})
		if err != nil && !errors.Is(err, run.ErrAlreadyRecorded) {
			return nil, err
		}
		p, err := decodePosted(stored.Output)
		if err != nil {
			return nil, err
		}
		f = found{posted: p, replyKey: eventKey("", in.ID)}
	}
	if err := sameRequest(rec, f.posted); err != nil {
		return nil, err
	}
	if stored, ok := byKey[f.replyKey]; ok {
		reply, err := replyFrom(in.ID, stored.Output)
		if err != nil {
			return nil, err
		}
		return delivered(reply), nil
	}
	return nil, types.ErrSuspended
}

// Reply records a decision or answer and wakes the owning run. It is
// idempotent by (ID, IdempotencyKey): the same reply again is a no-op, and a
// different reply to an answered interrupt returns ErrConflict. An unknown
// ID matches types.ErrInterruptNotFound and a late reply matches
// types.ErrInterruptExpired as well as ErrClosed.
func (rt *Router) Reply(ctx context.Context, reply types.InterruptReply) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if reply.ID == "" {
		return fmt.Errorf("%w: empty interrupt ID", types.ErrInterruptNotFound)
	}
	runID := rt.RunID
	if runID == "" {
		var err error
		if runID, err = rt.owner(ctx, reply.ID); err != nil {
			return err
		}
	}
	return rt.Engine.reply(ctx, runID, reply)
}

// owner finds the unfinished run holding interrupt id.
func (rt *Router) owner(ctx context.Context, id string) (string, error) {
	const batch = 256
	var (
		owners []string
		cursor run.PendingCursor
	)
	before := time.Now().Add(time.Second)
	for {
		runs, err := rt.Engine.lgr.ListPending(ctx, before, cursor, batch)
		if err != nil {
			return "", err
		}
		for _, p := range runs {
			_, records, err := rt.Engine.lgr.Load(ctx, p.ID)
			if err != nil {
				return "", err
			}
			all, _, err := interrupts(records)
			if err != nil {
				return "", err
			}
			if _, ok := all[id]; ok {
				owners = append(owners, p.ID)
			}
		}
		if len(runs) < batch {
			break
		}
		last := runs[len(runs)-1]
		cursor = run.PendingCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	switch len(owners) {
	case 0:
		return "", fmt.Errorf("%w: %s", types.ErrInterruptNotFound, id)
	case 1:
		return owners[0], nil
	default:
		return "", fmt.Errorf("%w: interrupt %s exists in %d runs; set Router.RunID", ErrConflict, id, len(owners))
	}
}

// Pending lists unanswered, unexpired interrupts of runID, including those
// raised by delegated children, oldest first. An unknown run has none.
func (rt *Router) Pending(ctx context.Context, runID string) ([]types.Interrupt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r, records, err := rt.Engine.lgr.Load(ctx, runID)
	if errors.Is(err, run.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return inspect(r, records, time.Now()).Pending, nil
}

// State is an inspection snapshot of one run.
type State struct {
	RunID    string
	Workflow string
	Status   run.RunStatus
	Error    string
	// Pending lists unanswered, unexpired interrupts, oldest first.
	Pending []types.Interrupt
	// Uncertain lists steps that wait for Reconcile.
	Uncertain []Uncertain
}

// Uncertain is a step whose outcome is unknown.
type Uncertain struct {
	Step    string
	Error   string               // what the run saw, or why it stopped
	Receipt *types.BudgetReceipt // conservative charge saved before dispatch
}

// Inspect reads the state of run id. A run that was never started returns an
// error matching run.ErrNotFound.
func (e *Engine) Inspect(ctx context.Context, id string) (State, error) {
	r, records, err := e.lgr.Load(ctx, id)
	if err != nil {
		return State{}, err
	}
	return inspect(r, records, time.Now()), nil
}

// inspect derives the run state from its records. Records that fail to
// decode are skipped; replay reports them.
func inspect(r run.Run, records []run.Record, now time.Time) State {
	s := State{RunID: r.ID, Workflow: r.Name, Status: r.Status, Error: r.Error}
	all, byKey, err := interrupts(records)
	if err == nil && !r.Status.Terminal() {
		for id, f := range all {
			if _, answered := byKey[f.replyKey]; answered || f.posted.Interrupt.Expired(now) {
				continue
			}
			s.Pending = append(s.Pending, asInterrupt(id, f.posted))
		}
	}
	sort.Slice(s.Pending, func(i, j int) bool {
		if !s.Pending[i].CreatedAt.Equal(s.Pending[j].CreatedAt) {
			return s.Pending[i].CreatedAt.Before(s.Pending[j].CreatedAt)
		}
		return s.Pending[i].ID < s.Pending[j].ID
	})
	if !r.Status.Terminal() {
		for _, u := range uncertainSteps(records, byKey) {
			s.Uncertain = append(s.Uncertain, u.Uncertain)
		}
	}
	sort.Slice(s.Uncertain, func(i, j int) bool { return s.Uncertain[i].Step < s.Uncertain[j].Step })
	return s
}

// asInterrupt presents a recorded interrupt. Approvals carry their markers
// and the tool call as payload.
func asInterrupt(id string, p postedRecord) types.Interrupt {
	in := p.Interrupt
	in.ID = id
	in.Payload = compactJSON(in.Payload)
	in.Markers = append([]types.Marker(nil), in.Markers...)
	in.Path = append([]string(nil), in.Path...)
	return in
}

// openStep is an uncertain step that has no reconciliation yet.
type openStep struct {
	Uncertain
	eventKey string
}

// uncertainSteps finds uncertainty markers without a reconcile event. A step
// retried after reconciliation can be uncertain again under a later
// occurrence; only the open occurrence is returned.
func uncertainSteps(records []run.Record, byKey map[string]run.Record) []openStep {
	var out []openStep
	for _, rec := range records {
		if rec.Kind != run.KindStep || !strings.HasPrefix(rec.Name, uncertainPrefix) {
			continue
		}
		name := strings.TrimPrefix(rec.Name, uncertainPrefix)
		i := strings.LastIndexByte(rec.Key, '#')
		if i < 0 {
			continue
		}
		occurrence := rec.Key[i+1:]
		if _, err := strconv.Atoi(occurrence); err != nil {
			continue
		}
		prefix, ok := strings.CutSuffix(rec.Key, rec.Name+"#"+occurrence)
		if !ok {
			continue
		}
		event := prefix + "event:" + reconcilePrefix + name + "#" + occurrence
		if _, done := byKey[event]; done {
			continue
		}
		u := openStep{Uncertain: Uncertain{Step: name}, eventKey: event}
		_ = json.Unmarshal(rec.Output, &u.Error)
		// The reservation of the uncertain round sits under the step's key
		// of the same occurrence.
		if res, ok := byKey[prefix+stepPrefix+name+"#"+occurrence+"/"+reserveName+"#0"]; ok {
			var raw []byte
			var receipt types.BudgetReceipt
			if json.Unmarshal(res.Output, &raw) == nil && decode(raw, &receipt) == nil {
				u.Receipt = &receipt
			}
		}
		out = append(out, u)
	}
	return out
}

// Reconcile resolves an uncertain step after the host checks its external
// effect. A supplied result is memoized as the step's result; nil permits
// another attempt, keeping the uncertain attempt's budget receipt. The run
// is enqueued to continue. A step that is not waiting for reconciliation
// returns ErrConflict.
func (e *Engine) Reconcile(ctx context.Context, id, step string, result *types.StepResult) error {
	r, records, err := e.lgr.Load(ctx, id)
	if err != nil {
		return err
	}
	if r.Status.Terminal() {
		return ErrClosed
	}
	byKey := make(map[string]run.Record, len(records))
	for _, rec := range records {
		byKey[rec.Key] = rec
	}
	var open *openStep
	for _, u := range uncertainSteps(records, byKey) {
		if u.Step == step {
			open = &u
			break
		}
	}
	if open == nil {
		return fmt.Errorf("%w: step %s is not waiting for reconciliation", ErrConflict, step)
	}
	var p reconcilePayload
	if result != nil {
		resolved := *result
		if resolved.Receipt == nil {
			resolved.Receipt = open.Receipt
		}
		if p.Result, err = encode(resolved); err != nil {
			return err
		}
	} else if open.Receipt != nil {
		if p.Receipt, err = encode(*open.Receipt); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = e.lgr.Record(ctx, run.Record{
		RunID: id, Key: open.eventKey, Kind: run.KindEvent, Name: reconcilePrefix + step,
		Codec: run.JSONCodec{}.ContentType(), Status: run.RecordOK, Output: raw,
	})
	if errors.Is(err, run.ErrAlreadyRecorded) {
		return fmt.Errorf("%w: step %s was already reconciled", ErrConflict, step)
	}
	if err != nil {
		return err
	}
	if err := e.q.Enqueue(ctx, id, 0); err != nil {
		return fmt.Errorf("reconciliation is recorded; enqueue run %s: %w", id, err)
	}
	return nil
}
