// Package local implements durable, resumable runs on a local filesystem.
// A nonblocking process lock gives each run one owner. Atomic, synced snapshots
// record each attempt before execution and each result before publication.
// It requires a filesystem with reliable advisory locks and atomic rename.
package local

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/urmzd/saige/agent"
	_ "github.com/urmzd/saige/agent/internal/durablecodec"
	"github.com/urmzd/saige/agent/types"
)

const (
	statusRunning   = "running"
	statusCancelled = "cancelled"
	statusCompleted = "completed"
)

var (
	ErrBusy          = errors.New("durable run already has a worker")
	ErrIndeterminate = errors.New("attempt outcome requires reconciliation")
	ErrConflict      = errors.New("durable run identity or decision conflict")
	ErrClosed        = errors.New("durable run cancelled or completed")
)

// Factory must return a fresh agent and fresh budget for every replay. The
// revision binds its model profiles, tool implementations and policy settings.
// Completed steps reconstruct conversation state without repeating operations.
type Factory func() *agent.Agent

type Engine struct {
	Directory   string
	ApprovalTTL time.Duration
}

// New uses a host-owned private directory. A zero TTL defaults to 24 hours.
func New(directory string) *Engine { return &Engine{Directory: directory, ApprovalTTL: 24 * time.Hour} }

type Step struct {
	Status      string    `json:"status"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
	Result      []byte    `json:"result,omitempty"` // gob preserves sealed content and raw bytes
	Error       string    `json:"error,omitempty"`
}

type Interrupt struct {
	Request     types.ApprovalRequest   `json:"request"`
	CreatedAt   time.Time               `json:"created_at"`
	ExpiresAt   time.Time               `json:"expires_at"`
	Decision    *types.ApprovalDecision `json:"decision,omitempty"`
	DecisionKey string                  `json:"decision_key,omitempty"`
	DecidedAt   time.Time               `json:"decided_at,omitempty"`
}

// State is a detached inspection snapshot. Input and step Result are gob bytes.
// Do not put credentials in input, tool arguments, or approval messages.
type Event struct {
	Sequence int       `json:"sequence"`
	At       time.Time `json:"at"`
	Kind     string    `json:"kind"`
	ID       string    `json:"id,omitempty"`
}

type State struct {
	History            []Event               `json:"history"`
	ReconciledReceipts []types.BudgetReceipt `json:"reconciled_receipts,omitempty"`
	Version            int                   `json:"version"`
	RunID              string                `json:"run_id"`
	Revision           string                `json:"revision"`
	Status             string                `json:"status"`
	Input              []byte                `json:"input"`
	Steps              map[string]Step       `json:"steps"`
	Interrupts         map[string]Interrupt  `json:"interrupts"`
	UpdatedAt          time.Time             `json:"updated_at"`
	Error              string                `json:"error,omitempty"`
}

type runner struct {
	mu       sync.Mutex
	state    State
	path     string
	ttl      time.Duration
	poisoned error
}

// Run owns a worker lease only for this call. ErrSuspended means pending
// approvals are saved; call Decide and then Run with the same identity/input.
// A crash during an operation leaves an indeterminate attempt. Run will not
// repeat it until Reconcile supplies its result or explicitly permits retry.
func (e *Engine) Run(ctx context.Context, id, revision string, factory Factory, input []types.Message) (*types.AssistantMessage, error) {
	if id == "" || revision == "" || factory == nil {
		return nil, errors.New("run ID, revision and factory are required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, release, err := e.acquire(id)
	if err != nil {
		return nil, err
	}
	defer release()
	state, err := readState(path)
	if errors.Is(err, os.ErrNotExist) {
		raw, encErr := encode(input)
		if encErr != nil {
			return nil, encErr
		}
		state = State{Version: 1, RunID: id, Revision: revision, Status: "ready", Input: raw, Steps: map[string]Step{}, Interrupts: map[string]Interrupt{}}
	} else if err != nil {
		return nil, err
	}
	if state.RunID != id || state.Revision != revision {
		return nil, ErrConflict
	}
	var saved []types.Message
	if err := decode(state.Input, &saved); err != nil {
		return nil, err
	}
	// Gob normalizes interface representations; compare the original logical values.
	currentRaw, err := encode(input)
	if err != nil {
		return nil, err
	}
	var current []types.Message
	if err := decode(currentRaw, &current); err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(saved, current) {
		return nil, fmt.Errorf("%w: input changed", ErrConflict)
	}
	if state.Status == statusCancelled {
		return nil, ErrClosed
	}
	r := &runner{state: state, path: path, ttl: e.ApprovalTTL}
	if r.ttl <= 0 {
		r.ttl = 24 * time.Hour
	}
	r.state.Status = statusRunning
	r.note("run.started", id)
	if err := r.save(); err != nil {
		return nil, err
	}
	a := factory()
	if a == nil {
		return nil, errors.New("factory returned nil agent")
	}
	if b := a.Budget(); b != nil {
		for _, receipt := range state.ReconciledReceipts {
			if err := b.Restore(receipt); err != nil {
				return nil, err
			}
		}
	}
	result, runErr := a.RunDurable(ctx, r, saved, "")
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case errors.Is(runErr, types.ErrSuspended):
		r.state.Status = "suspended"
	case runErr != nil:
		r.state.Status = "failed"
	default:
		r.state.Status = statusCompleted
	}
	r.state.Error = ""
	if runErr != nil {
		r.state.Error = runErr.Error()
	}
	r.note("run."+r.state.Status, id)
	if err := r.save(); err != nil {
		return nil, err
	}
	return result, runErr
}

func (r *runner) ConcurrentSteps() bool { return true }

func (r *runner) SharedBudgetOnly() bool { return true }

func (r *runner) RunStep(ctx context.Context, name string, fn func(context.Context) (types.StepResult, error)) (types.StepResult, error) {
	r.mu.Lock()
	if r.poisoned != nil {
		err := r.poisoned
		r.mu.Unlock()
		return types.StepResult{}, err
	}
	if s, ok := r.state.Steps[name]; ok {
		r.mu.Unlock()
		if s.Status != statusCompleted {
			return types.StepResult{}, fmt.Errorf("%w: %s: %s", ErrIndeterminate, name, s.Error)
		}
		var result types.StepResult
		err := decode(s.Result, &result)
		return result, err
	}
	if err := ctx.Err(); err != nil {
		r.mu.Unlock()
		return types.StepResult{}, err
	}
	s := Step{Status: statusRunning, StartedAt: time.Now().UTC()}
	r.state.Steps[name] = s
	r.note("step.started", name)
	if err := r.save(); err != nil {
		r.mu.Unlock()
		return types.StepResult{}, err
	}
	r.mu.Unlock()
	result, err := safeStep(ctx, fn)
	r.mu.Lock()
	defer r.mu.Unlock()
	if errors.Is(err, types.ErrSuspended) || errors.Is(err, types.ErrBudgetAdmission) {
		delete(r.state.Steps, name)
		r.note("step.not-dispatched", name)
		if saveErr := r.save(); saveErr != nil {
			return types.StepResult{}, saveErr
		}
		return types.StepResult{}, err
	}
	s.CompletedAt = time.Now().UTC()
	s.Status = statusCompleted
	encoded, encodeErr := encode(result)
	s.Result = encoded
	if encodeErr != nil {
		// Keep the pre-dispatch reservation if the result cannot be serialized.
		s.Result = r.state.Steps[name].Result
	}
	if err == nil {
		err = encodeErr
	}
	if err != nil {
		s.Status = "indeterminate"
		s.Error = err.Error()
	}
	r.note("step."+s.Status, name)

	r.state.Steps[name] = s
	if saveErr := r.save(); saveErr != nil {
		return types.StepResult{}, saveErr
	}
	if err != nil {
		return types.StepResult{}, err
	}
	// Decode the committed value so callers do not share producer-owned maps.
	var detached types.StepResult
	if err := decode(s.Result, &detached); err != nil {
		return types.StepResult{}, err
	}
	return detached, nil
}

func safeStep(ctx context.Context, fn func(context.Context) (types.StepResult, error)) (result types.StepResult, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("step panic: %v", p)
		}
	}()
	return fn(ctx)
}

func (r *runner) ResolveApproval(ctx context.Context, req types.ApprovalRequest) (types.ApprovalDecision, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return types.ApprovalDecision{}, err
	}
	if r.poisoned != nil {
		return types.ApprovalDecision{}, r.poisoned
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return types.ApprovalDecision{}, err
	}
	if pending, ok := r.state.Interrupts[req.ID]; ok {
		old, _ := json.Marshal(pending.Request)
		if !bytes.Equal(raw, old) {
			return types.ApprovalDecision{}, ErrConflict
		}
		if pending.Decision != nil {
			raw, err := json.Marshal(pending.Decision)
			if err != nil {
				return types.ApprovalDecision{}, err
			}
			var copy types.ApprovalDecision
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.UseNumber()
			err = decoder.Decode(&copy)
			return copy, err
		}
		if !time.Now().Before(pending.ExpiresAt) {
			return types.ApprovalDecision{}, fmt.Errorf("%w: approval expired", ErrClosed)
		}
		return types.ApprovalDecision{}, types.ErrSuspended
	}
	var detached types.ApprovalRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&detached); err != nil {
		return types.ApprovalDecision{}, err
	}
	now := time.Now().UTC()
	r.state.Interrupts[req.ID] = Interrupt{Request: detached, CreatedAt: now, ExpiresAt: now.Add(r.ttl)}
	r.note("approval.requested", req.ID)
	if err := r.save(); err != nil {
		return types.ApprovalDecision{}, err
	}
	return types.ApprovalDecision{}, types.ErrSuspended
}

// Inspect reads an atomic, detached snapshot without acquiring a worker lease.
func (e *Engine) Inspect(id string) (State, error) { return readState(e.path(id)) }

// Decide accepts one authenticated host decision. key is its idempotency key.
// A conflicting retry, stale revision, cancellation, or expiry is rejected.
func (e *Engine) Decide(id, revision, interruptID, key string, decision types.ApprovalDecision) error {
	if key == "" {
		return errors.New("decision idempotency key required")
	}
	return e.update(id, revision, func(s *State) error {
		if s.Status == statusCompleted || s.Status == statusCancelled {
			return ErrClosed
		}
		p, ok := s.Interrupts[interruptID]
		if !ok {
			return errors.New("unknown interrupt")
		}
		if p.Decision != nil {
			a, _ := json.Marshal(p.Decision)
			b, err := json.Marshal(decision)
			if err == nil && key == p.DecisionKey && bytes.Equal(a, b) {
				return nil
			}
			return ErrConflict
		}
		if !time.Now().Before(p.ExpiresAt) {
			return fmt.Errorf("%w: approval expired", ErrClosed)
		}
		p.Decision = &decision
		p.DecisionKey = key
		p.DecidedAt = time.Now().UTC()
		s.Interrupts[interruptID] = p
		s.History = append(s.History, Event{Sequence: len(s.History) + 1, At: time.Now().UTC(), Kind: "approval.decided", ID: interruptID})
		return nil
	})
}

// Cancel prevents future resumes. A currently leased run returns ErrBusy; its
// owner must cancel the Run context first, then commit cancellation here.
func (e *Engine) Cancel(id, revision string) error {
	return e.update(id, revision, func(s *State) error {
		if s.Status == statusCompleted {
			return ErrClosed
		}
		if s.Status == statusCancelled {
			return nil
		}
		s.Status = statusCancelled
		s.History = append(s.History, Event{Sequence: len(s.History) + 1, At: time.Now().UTC(), Kind: "run.cancelled", ID: id})
		return nil
	})
}

// Reconcile resolves an uncertain operation after the host checks its external
// effect. A supplied result is memoized; nil explicitly permits another attempt.
// No automatic retry is safe for an operation with an unknown side effect.
func (e *Engine) Reconcile(id, revision, name string, result *types.StepResult) error {
	return e.update(id, revision, func(s *State) error {
		step, ok := s.Steps[name]
		if !ok || step.Status == statusCompleted {
			return ErrConflict
		}
		var prior types.StepResult
		if len(step.Result) > 0 {
			if err := decode(step.Result, &prior); err != nil {
				return err
			}
		}
		s.History = append(s.History, Event{Sequence: len(s.History) + 1, At: time.Now().UTC(), Kind: "step.reconciled", ID: name})
		if s.Status == statusCancelled || s.Status == statusCompleted {
			return ErrClosed
		}
		if result == nil {
			if prior.Receipt != nil {
				s.ReconciledReceipts = append(s.ReconciledReceipts, *prior.Receipt)
			}
			delete(s.Steps, name)
			return nil
		}
		copy := *result
		if copy.Receipt == nil {
			copy.Receipt = prior.Receipt
		}
		raw, err := encode(copy)
		if err != nil {
			return err
		}
		step.Status = statusCompleted
		step.Result = raw
		step.Error = ""
		step.CompletedAt = time.Now().UTC()
		s.Steps[name] = step
		return nil
	})
}

func (e *Engine) update(id, revision string, fn func(*State) error) error {
	path, release, err := e.acquire(id)
	if err != nil {
		return err
	}
	defer release()
	s, err := readState(path)
	if err != nil {
		return err
	}
	if s.RunID != id || s.Revision != revision {
		return ErrConflict
	}
	if err := fn(&s); err != nil {
		return err
	}
	r := runner{state: s, path: path}
	return r.save()
}

func (e *Engine) path(id string) string {
	sum := sha256.Sum256([]byte(id))
	return filepath.Join(e.Directory, hex.EncodeToString(sum[:]), "state.json")
}
func (e *Engine) acquire(id string) (string, func(), error) {
	if e.Directory == "" || id == "" {
		return "", nil, errors.New("directory and run ID required")
	}
	path := e.path(id)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil { // #nosec G703 -- host-selected root plus a SHA-256 run ID, never an input path.
		return "", nil, err
	}
	release, err := lock(filepath.Join(filepath.Dir(path), "worker.lock"))
	return path, release, err
}

func readState(path string) (State, error) {
	var s State
	raw, err := os.ReadFile(path) // #nosec G304 -- path is derived from the host directory and a SHA-256 run ID.
	if err != nil {
		return s, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&s); err != nil {
		return s, err
	}
	if s.Version != 1 || s.Steps == nil || s.Interrupts == nil {
		return s, errors.New("invalid durable state")
	}
	return s, nil
}
func (r *runner) save() error {
	if r.poisoned != nil {
		return r.poisoned
	}
	r.state.UpdatedAt = time.Now().UTC()
	raw, err := json.MarshalIndent(r.state, "", "  ")
	if err == nil {
		err = atomicWrite(r.path, raw)
	}
	if err != nil {
		r.poisoned = err
	}
	return err
}
func atomicWrite(path string, raw []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".snapshot-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}
func encode(v any) ([]byte, error) {
	var b bytes.Buffer
	err := gob.NewEncoder(&b).Encode(v)
	return b.Bytes(), err
}
func decode(raw []byte, v any) error { return gob.NewDecoder(bytes.NewReader(raw)).Decode(v) }

func (r *runner) note(kind, id string) {
	r.state.History = append(r.state.History, Event{Sequence: len(r.state.History) + 1, At: time.Now().UTC(), Kind: kind, ID: id})
}

// RecordReservation commits recovery accounting before the external request.
func (r *runner) RecordReservation(ctx context.Context, name string, receipt types.BudgetReceipt) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.poisoned != nil {
		return r.poisoned
	}
	step, ok := r.state.Steps[name]
	if !ok || step.Status != statusRunning {
		return ErrConflict
	}
	raw, err := encode(types.StepResult{Receipt: &receipt})
	if err != nil {
		return err
	}
	step.Result = raw
	r.state.Steps[name] = step
	r.note("budget.reserved", name)
	return r.save()
}
