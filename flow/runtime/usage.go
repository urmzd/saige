package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/urmzd/saige/durable"
	"github.com/urmzd/saige/durable/run"

	"github.com/urmzd/saige/flow/event"
)

// # Usage
//
// A run's usage is counted from the usage.call and usage.child events its
// attempts publish, never from a figure an executor returns: the events are
// reported as each call completes, so they exist for a turn that fails or is
// stopped halfway, and a total built from them always equals the sum of the
// calls a subscriber saw.
//
// What is counted, across durable's retries and replays:
//
//   - Every model call any attempt made, once. Each attempt writes the calls
//     it relayed to the ledger (usageRecordName) before it returns, whether it
//     failed or succeeded: a failure is retried, and so is a success whose
//     record was never written. The attempt that finally records an outcome
//     reads the earlier attempts' records and counts them with its own. The
//     record is per attempt and write-once.
//   - Nothing for a replay. The agent activity's outcome is memoized, so a
//     replay of the workflow body reads the recorded total and calls no model.
//   - A child run once, however many attempts of the parent asked for it: its
//     report is keyed by the child's run id.
//
// Keys are "<run>/a<attempt>[/<agent>.<sub>...]/<n>", which no other call of
// any attempt can share. saige's own AccountingID is carried beside it and
// never counted by. event.Tally counts each key once, which is what makes a
// report delivered or relayed twice harmless.
//
// What is not counted: an attempt that dies without returning (a crash, a
// worker killed mid-turn) writes no record, so its calls are on the bus and
// not in the total; and a superseded attempt that keeps running after another
// has taken the run over may record after the run's outcome read the records.
//
// The records need the ledger the runs execute from (WithStopLedger). Without
// it the total counts the attempt that produced the outcome and the per-call
// events still report every call.

// usageRecordName is the ledger record an attempt that failed after spending
// tokens writes its calls to: "fiber.usage.a<attempt>", as an event record
// "event:fiber.usage.a<attempt>#0", the shape the ledger expects from
// outside a call. The workflow body claims the ones the outcome counted.
const usageRecordName = "fiber.usage.a"

func usageRecordKey(attempt int) string {
	return "event:" + usageRecordName + strconv.Itoa(attempt) + "#0"
}

// spent is what one attempt reported, as its usage record holds it.
type spent struct {
	Attempt  int                `json:"attempt"`
	Calls    []event.UsageCall  `json:"calls,omitempty"`
	Children []event.UsageChild `json:"children,omitempty"`
}

func (s *spent) empty() bool { return len(s.Calls) == 0 && len(s.Children) == 0 }

// observe files a usage body the attempt relayed, and ignores every other
// type. The record keeps the usage in the shape it always had, so the tally
// keys a call and a child exactly as before.
func (s *spent) observe(b event.Body) {
	switch b := b.(type) {
	case event.UsageCall:
		s.Calls = append(s.Calls, b)
	case event.UsageChild:
		s.Children = append(s.Children, b)
	}
}

func (s *spent) into(t *event.Tally) {
	for _, c := range s.Calls {
		t.Call(c)
	}
	for _, c := range s.Children {
		t.Child(c)
	}
}

// meter is spent, safe to observe from the executor's goroutines.
type meter struct {
	mu sync.Mutex
	s  spent
}

func (m *meter) observe(b event.Body) {
	switch b.(type) {
	case event.UsageCall, event.UsageChild:
	default:
		return
	}
	m.mu.Lock()
	m.s.observe(b)
	m.mu.Unlock()
}

func (m *meter) snapshot() spent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return spent{
		Attempt:  m.s.Attempt,
		Calls:    append([]event.UsageCall(nil), m.s.Calls...),
		Children: append([]event.UsageChild(nil), m.s.Children...),
	}
}

// priorUsage reads what earlier attempts of a run spent and failed: their
// usage records, in attempt order, and the attempts they belong to.
//
// It runs inside the agent activity, whose result is memoized, so what it read
// is recorded with the outcome and a replay does not read again. A ledger that
// cannot be read is logged and the earlier attempts are left out: a run is not
// failed over its accounting, and their calls are still on the bus.
func (r *Runtime) priorUsage(ctx context.Context, runID string, attempt int) ([]spent, []int) {
	if r.stops == nil || attempt <= 1 {
		return nil, nil
	}
	_, records, err := r.recordsOf(ctx, runID, false, prefixUsage)
	if err != nil {
		r.logger.Warn("flow/runtime: could not read the usage of earlier attempts; the total counts this one",
			"run_id", runID, "error", err)
		return nil, nil
	}
	prior := usageRecords(records)
	var out []spent
	var attempts []int
	for _, s := range prior {
		if s.Attempt < attempt {
			out = append(out, s)
			attempts = append(attempts, s.Attempt)
		}
	}
	return out, attempts
}

// usageRecords decodes the usage records among a run's records, in attempt
// order.
func usageRecords(records []run.Record) []spent {
	var out []spent
	for _, rec := range records {
		if !strings.HasPrefix(rec.Key, "event:"+usageRecordName) || rec.Status != run.RecordOK {
			continue
		}
		var s spent
		if err := json.Unmarshal(rec.Output, &s); err != nil || usageRecordKey(s.Attempt) != rec.Key {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Attempt < out[j].Attempt })
	return out
}

// recordSpent writes what an attempt spent on the run, so the attempt that
// finally records an outcome counts it, and reports whether the record is
// there. Write-once: a second write for the same attempt keeps the first.
func (r *Runtime) recordSpent(ctx context.Context, runID string, s spent) bool {
	if r.stops == nil || s.empty() {
		return false
	}
	body, err := json.Marshal(s)
	if err == nil {
		_, err = r.stops.Record(context.WithoutCancel(ctx), run.Record{
			RunID: runID, Key: usageRecordKey(s.Attempt), Kind: run.KindEvent,
			Name: usageRecordName + strconv.Itoa(s.Attempt), Codec: "application/json",
			Status: run.RecordOK, Output: body, Attempt: s.Attempt,
		})
	}
	if err != nil && !errors.Is(err, run.ErrAlreadyRecorded) {
		r.logger.Warn("flow/runtime: could not record an attempt's usage; if the attempt is executed again its calls are not in the total",
			"run_id", runID, "attempt", s.Attempt, "calls", len(s.Calls), "error", err)
		return false
	}
	return true
}

// claimUsage reads, from the workflow body, the usage records the agent's
// outcome counted, so the ledger keeps no record that no call read. Which ones
// is decided by the memoized outcome, so every replay claims the same set. The
// claim is advisory: the total is already decided.
//
// A run that parked has an outcome per turn, and each counts every earlier
// attempt: claimed holds what this body has claimed already, since a second
// claim of one record would read an occurrence that does not exist.
func (r *Runtime) claimUsage(ctx context.Context, in Input, attempts []int, claimed map[int]bool) {
	for _, a := range attempts {
		if claimed[a] {
			continue
		}
		claimed[a] = true
		_, err := durable.Event[json.RawMessage](ctx, usageRecordName+strconv.Itoa(a))
		switch {
		case err == nil, errors.Is(err, run.ErrParked):
		default:
			r.logger.Warn("flow/runtime: could not claim a usage record",
				"run_id", in.runID(), "attempt", a, "error", err)
		}
	}
}

// RecordedUsage is what a run spent, as its ledger records hold it. It is for
// a run that has no output to read it from: one that failed. For a run that
// ended any other way, Output.Usage is the same figure.
//
// The agent's recorded outcome holds the total when the agent finished and
// something after it failed the run. Otherwise the total is the usage records
// of the attempts that failed, which is every model call the run made. It
// reports false when the records hold neither.
func RecordedUsage(records []run.Record) (event.RunUsage, bool) {
	// A run that parked has an outcome per turn; the latest counts every
	// one before it.
	var best *event.RunUsage
	bestAttempt := -1
	for _, rec := range records {
		if (rec.Name != ActivityAgent && rec.Name != ActivityResume) || rec.Kind != run.KindActivity || rec.Status != run.RecordOK {
			continue
		}
		var out Outcome
		if json.Unmarshal(rec.Output, &out) == nil && out.Usage != nil && rec.Attempt > bestAttempt {
			best, bestAttempt = out.Usage, rec.Attempt
		}
	}
	prior := usageRecords(records)
	if best != nil {
		// An attempt after the latest outcome executed a turn that failed:
		// a continuation of a parked turn, say. Every attempt that executed
		// records its calls, so the records are then the whole total.
		later := false
		for _, s := range prior {
			later = later || s.Attempt > bestAttempt
		}
		if !later {
			return *best, true
		}
	}
	if len(prior) == 0 {
		return event.RunUsage{}, false
	}
	var t event.Tally
	for i := range prior {
		prior[i].into(&t)
	}
	return t.Usage(), true
}

// usageReporterKey carries, on a Delegator's context, how a child run's usage
// is put on the parent's stream.
type usageReporterKey struct{}

// childUsageKey carries, on the context a turn runtime is called with, how a
// child run reported by a Delegator is checked and read: see ReportChildUsage.
type childUsageKey struct{}

// childUsageOf returns the check the runtime put on ctx, or nil.
func childUsageOf(ctx context.Context) func(context.Context, string) (event.UsageChild, error) {
	f, _ := ctx.Value(childUsageKey{}).(func(context.Context, string) (event.UsageChild, error))
	return f
}

// ErrNotAChild reports a run a Delegator cannot roll up: not in the ledger,
// another tenant's, the parent itself, or not ended yet.
var ErrNotAChild = errors.New("runtime: not a child run that can be rolled up")

// childUsage reads a child run's total from the ledger for the parent run
// executing under tenant, and refuses anything it cannot vouch for. Nothing the
// host says about the child is trusted but its run id: the run must exist in
// the ledger, belong to the parent's tenant, not be the parent, and have
// ended, and its total and agent are read from its own record. Another
// tenant's run is refused with the same error as a run that does not exist.
func (r *Runtime) childUsage(tenant, parentRunID string) func(context.Context, string) (event.UsageChild, error) {
	return func(ctx context.Context, runID string) (event.UsageChild, error) {
		runID = strings.TrimSpace(runID)
		if r.stops == nil {
			return event.UsageChild{}, fmt.Errorf("%w: rolling up a child run needs the run ledger (WithStopLedger)", ErrNotAChild)
		}
		if runID == "" || runID == parentRunID {
			return event.UsageChild{}, fmt.Errorf("%w: %q", ErrNotAChild, runID)
		}
		rec, records, err := r.stops.Load(ctx, runID)
		switch {
		case errors.Is(err, run.ErrNotFound):
			return event.UsageChild{}, fmt.Errorf("%w: no run %q", ErrNotAChild, runID)
		case err != nil:
			return event.UsageChild{}, fmt.Errorf("runtime: read child run %s: %w", runID, err)
		}
		var in Input
		if err := json.Unmarshal(rec.Input, &in); err != nil || in.Trigger.Tenant != tenant {
			return event.UsageChild{}, fmt.Errorf("%w: no run %q", ErrNotAChild, runID)
		}
		if in.Session != nil && rec.ParentRunID != parentRunID {
			return event.UsageChild{}, fmt.Errorf("%w: run %q has another parent", ErrNotAChild, runID)
		}
		if !rec.Status.Terminal() {
			return event.UsageChild{}, fmt.Errorf("%w: run %q has not ended", ErrNotAChild, runID)
		}
		var total event.Usage
		if rec.Status == run.StatusSucceeded {
			var out Output
			if err := json.Unmarshal(rec.Output, &out); err == nil && out.Usage != nil {
				total = out.Usage.Total
			}
		} else if u, ok := RecordedUsage(records); ok {
			total = u.Total
		}
		return event.UsageChild{
			AccountingID: event.ChildRunAccountingID(runID),
			RunID:        runID,
			Agent:        in.Subscriber,
			Depth:        1,
			Usage:        total,
		}, nil
	}
}

// ReportChildUsage rolls a child run up into the run whose turn delegated to
// it. A Delegator calls it with the context it was given, once the run it
// started for the delegation has ended:
//
//	out, err := rt.Result(ctx, childRunID)
//	...
//	err = runtime.ReportChildUsage(ctx, childRunID)
//
// Only the run id is taken from the host. The runtime reads the child from the
// ledger, refuses it with ErrNotAChild unless it is a run of the parent's
// tenant other than the parent that has ended, and reads its total (its own
// calls and everything it delegated) from its record. It then publishes a
// usage.child event on the parent's stream, and the parent's total counts it
// in Children: once, however many times it is reported or however many
// attempts of the parent delegated to it, keyed by its run id.
//
// It needs the runtime to have the run ledger (WithStopLedger) and the
// executor to be a Sandbox. It returns ErrInvalid outside a Delegator's
// context.
func ReportChildUsage(ctx context.Context, runID string) error {
	report, ok := ctx.Value(usageReporterKey{}).(func(context.Context, string) error)
	if !ok || report == nil {
		return fmt.Errorf("%w: ReportChildUsage is called with the context a Delegator was given", ErrInvalid)
	}
	return report(ctx, runID)
}

// relayUsage rewrites a usage body that came up from a delegated sub-task so
// that it reads one level deeper, and reports false for any other type: a
// sub-task's conversation is its parent's tool result, never its stream.
func relayUsage(b event.Body) (event.Body, bool) {
	switch v := b.(type) {
	case event.UsageCall:
		v.Depth++
		return v, true
	case event.UsageChild:
		v.Depth++
		return v, true
	default:
		return nil, false
	}
}

// accountingOf is the key prefix a turn's calls are reported under: the run
// and the attempt, or for a sub-task the prefix its parent handed it.
func accountingOf(req Request) string {
	if req.Accounting != "" {
		return req.Accounting
	}
	return req.RunID + "/a" + strconv.Itoa(max(1, req.Attempt))
}
