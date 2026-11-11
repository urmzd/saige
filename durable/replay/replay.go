// Package replay is durable's correctness heart: the memoized re-execution
// of a run. A worker builds one Frame per claim from the run's ledger
// records; every wrapped call in user code bottoms out in Frame.Do, which
// either returns a recorded result instantly (replay) or executes for real
// and records the result before releasing it (frontier). Crash anywhere,
// re-claim, and the run resumes at the first unrecorded call.
//
// Record identity follows the activity, not its position: the Kth call to a
// name yields "name#K". Inserting, removing, or reordering differently-named
// calls between releases leaves existing checkpoints valid; a same-key call
// whose kind or input diverged fails loudly with run.ErrNonDeterministic.
package replay

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/urmzd/saige/durable/ledger"
	"github.com/urmzd/saige/durable/queue"
	"github.com/urmzd/saige/durable/run"
)

// Frame is the replay state of one claimed attempt. It is built once per
// claim and carried through user code inside context.Context. The workflow
// body is single-goroutine in v1; the frame is nonetheless mutex-guarded so
// violations stay memory-safe and surface as loud determinism errors, never
// corruption.
type Frame struct {
	runID   string
	attempt int
	lgr     ledger.Ledger
	deltas  queue.DeltaLog // nil when the queue has no DeltaLog capability
	codec   run.Codec

	mu       sync.Mutex
	memo     map[string]run.Record
	consumed map[string]bool
	counts   map[string]int
	active   []string // stack of record keys currently executing (nested calls)
}

// NewFrame builds the frame for one claimed attempt from the run's records
// (the ledger.Load prefetch). deltas may be nil.
func NewFrame(runID string, attempt int, lgr ledger.Ledger, codec run.Codec, records []run.Record, deltas queue.DeltaLog) *Frame {
	memo := make(map[string]run.Record, len(records))
	for _, r := range records {
		memo[r.Key] = r
	}
	if codec == nil {
		codec = run.JSONCodec{}
	}
	return &Frame{
		runID:    runID,
		attempt:  attempt,
		lgr:      lgr,
		deltas:   deltas,
		codec:    codec,
		memo:     memo,
		consumed: make(map[string]bool, len(records)),
		counts:   make(map[string]int),
	}
}

// RunID is the ID of the run this frame re-executes.
func (f *Frame) RunID() string { return f.runID }

// Attempt is the claim attempt this frame executes under, the fencing token.
func (f *Frame) Attempt() int { return f.attempt }

// Codec is the codec this frame decodes and encodes records with.
func (f *Frame) Codec() run.Codec { return f.codec }

// ActiveKey returns the record key currently executing, or "" outside a
// wrapped call. Combined with the run ID it is the idempotency key handed to
// downstream systems: "{runID}:{key}".
func (f *Frame) ActiveKey() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.active) == 0 {
		return ""
	}
	return f.active[len(f.active)-1]
}

// Unconsumed returns record keys that were never reached by the time the
// workflow returned — typically calls removed by a code change. Advisory:
// the worker logs them; the computed result stands.
func (f *Frame) Unconsumed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keys []string
	for k := range f.memo {
		if !f.consumed[k] {
			keys = append(keys, k)
		}
	}
	return keys
}

// nextKey derives the correlated record key for a call: explicit user keys
// are order-independent ("k:<key>"); everything else is name#occurrence,
// with events namespaced ("event:<name>#<k>").
//
// Keys are scoped under the executing parent's key ("parent#0/child#1").
// This is load-bearing: a memoized parent's body never re-executes, so its
// children's calls never happen on replay — if children shared the parent's
// counter space, every later same-name call would silently shift by the
// skipped occurrences. Scoping makes the skipped subtree self-contained.
func (f *Frame) nextKey(kind run.Kind, name, userKey string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	prefix := ""
	if n := len(f.active); n > 0 {
		// The innermost active key already embeds its full ancestry
		// ("a#0/b#1"), so it alone is the prefix — joining the stack
		// would double every ancestor segment.
		prefix = f.active[n-1] + "/"
	}
	if userKey != "" {
		return prefix + "k:" + userKey
	}
	base := name
	if kind == run.KindEvent {
		base = "event:" + name
	}
	scoped := prefix + base
	n := f.counts[scoped]
	f.counts[scoped] = n + 1
	return scoped + "#" + strconv.Itoa(n)
}

// Do executes one wrapped call under replay semantics. input must already be
// marshaled (hashed for activities; nil for steps and events). fn is nil for
// events — an event is a read, and its absence parks the run.
func (f *Frame) Do(ctx context.Context, kind run.Kind, name, userKey string, input []byte, fn func(context.Context) ([]byte, error)) ([]byte, error) {
	key := f.nextKey(kind, name, userKey)

	inputHash := ""
	if kind == run.KindActivity {
		inputHash = run.HashInput(input)
	}

	f.mu.Lock()
	rec, hit := f.memo[key]
	if hit {
		f.consumed[key] = true
	}
	f.mu.Unlock()

	if hit {
		return f.replayHit(key, kind, name, inputHash, rec)
	}

	if kind == run.KindEvent {
		return nil, fmt.Errorf("replay: event %q (%s) has no record: %w", name, key, run.ErrParked)
	}

	// Frontier: execute for real.
	f.push(key)
	out, err := fn(ctx)
	f.pop()

	if err != nil {
		if !run.IsNonRetryable(err) {
			return nil, err // retryable: no record; the next attempt re-executes
		}
		// Non-retryable: the failure itself is memoized. The error is
		// returned in its RECORDED form even on this first attempt, so
		// user code branching on the failure sees the identical value on
		// every attempt — determinism outranks error-identity fidelity.
		failed := run.Record{
			RunID: f.runID, Key: key, Kind: kind, Name: name,
			InputHash: inputHash, Codec: f.codec.ContentType(),
			Status: run.RecordFailed, Error: err.Error(), Attempt: f.attempt,
		}
		stored, rerr := f.lgr.Record(ctx, failed)
		switch {
		case rerr == nil:
			f.remember(stored)
			f.seal(ctx, key)
			return nil, &run.RecordedError{Name: name, Key: key, Message: stored.Error}
		case errors.Is(rerr, run.ErrAlreadyRecorded):
			return f.adopt(key, kind, name, inputHash, stored)
		default:
			return nil, fmt.Errorf("replay: record %s: %w", key, rerr)
		}
	}

	stored, rerr := f.lgr.Record(ctx, run.Record{
		RunID: f.runID, Key: key, Kind: kind, Name: name,
		InputHash: inputHash, Codec: f.codec.ContentType(),
		Status: run.RecordOK, Output: out, Attempt: f.attempt,
	})
	switch {
	case rerr == nil:
		f.remember(stored)
		f.seal(ctx, key)
		return stored.Output, nil
	case errors.Is(rerr, run.ErrAlreadyRecorded):
		// A concurrent attempt won the race; adopt its truth so histories
		// converge on exactly one value per key.
		return f.adopt(key, kind, name, inputHash, stored)
	default:
		return nil, fmt.Errorf("replay: record %s: %w", key, rerr)
	}
}

// remember inserts a just-recorded (or adopted) record into the frame's memo
// so a repeated call under the same explicit key within this attempt replays
// instead of re-executing the side effect.
func (f *Frame) remember(rec run.Record) {
	f.mu.Lock()
	f.memo[rec.Key] = rec
	f.consumed[rec.Key] = true
	f.mu.Unlock()
}

// replayHit verifies a memoized record against the live call and returns its
// result. Verification failure is terminal: identical code against identical
// records cannot succeed, so it errors out rather than burning retries.
func (f *Frame) replayHit(key string, kind run.Kind, name, inputHash string, rec run.Record) ([]byte, error) {
	if err := f.verify(key, kind, name, inputHash, rec); err != nil {
		return nil, err
	}
	if rec.Status == run.RecordFailed {
		return nil, &run.RecordedError{Name: name, Key: key, Message: rec.Error}
	}
	return rec.Output, nil
}

// adopt takes a record another attempt stored first, verifies it, marks it
// consumed, and returns its result — discarding this attempt's own value.
func (f *Frame) adopt(key string, kind run.Kind, name, inputHash string, stored run.Record) ([]byte, error) {
	f.remember(stored)
	f.seal(context.Background(), key)
	return f.replayHit(key, kind, name, inputHash, stored)
}

func (f *Frame) verify(key string, kind run.Kind, name, inputHash string, rec run.Record) error {
	if rec.Kind != kind || rec.Name != name {
		return fmt.Errorf("replay: %s recorded as %s %q, replayed as %s %q: %w",
			key, rec.Kind, rec.Name, kind, name, run.ErrNonDeterministic)
	}
	if kind == run.KindActivity && rec.InputHash != inputHash {
		return fmt.Errorf("replay: %s (%s) input diverged from recorded input: %w",
			key, name, run.ErrNonDeterministic)
	}
	if rec.Codec != "" && rec.Codec != f.codec.ContentType() {
		// A payload recorded under one codec cannot be decoded by another;
		// retrying cannot fix a configuration mismatch.
		return run.NonRetryable(fmt.Errorf("replay: %s recorded with codec %q, replaying with %q",
			key, rec.Codec, f.codec.ContentType()))
	}
	return nil
}

// Emit appends a user delta to the run's stream, tagged with the record key
// currently executing. Advisory flow: no DeltaLog capability means no-op.
func (f *Frame) Emit(ctx context.Context, payload []byte) error {
	if f.deltas == nil {
		return nil
	}
	return f.deltas.Append(ctx, run.Delta{
		RunID: f.runID, RecordKey: f.ActiveKey(), Attempt: f.attempt,
		Kind: run.DeltaUser, Payload: payload,
	})
}

// PriorDeltas returns the payloads this record key emitted on previous
// attempts — the raw material for provider-side resume. The read is exposed;
// no magic is applied.
func (f *Frame) PriorDeltas(ctx context.Context) ([][]byte, error) {
	if f.deltas == nil {
		return nil, nil
	}
	key := f.ActiveKey()
	var out [][]byte
	cursor := queue.Cursor("")
	for {
		batch, next, err := f.deltas.Read(ctx, f.runID, cursor, 512)
		if err != nil {
			return nil, fmt.Errorf("replay: prior deltas: %w", err)
		}
		for _, d := range batch {
			if d.Kind == run.DeltaUser && d.RecordKey == key && d.Attempt < f.attempt {
				out = append(out, d.Payload)
			}
		}
		if len(batch) == 0 || next == cursor {
			return out, nil
		}
		cursor = next
	}
}

// seal appends the divider marking a record key as resolved by the ledger.
// Best-effort: the checkpoint is already durable; a lost seal only degrades
// stream trimming, never correctness.
func (f *Frame) seal(ctx context.Context, key string) {
	if f.deltas == nil {
		return
	}
	_ = f.deltas.Append(ctx, run.Delta{
		RunID: f.runID, RecordKey: key, Attempt: f.attempt, Kind: run.DeltaSeal,
	})
}

func (f *Frame) push(key string) {
	f.mu.Lock()
	f.active = append(f.active, key)
	f.mu.Unlock()
}

func (f *Frame) pop() {
	f.mu.Lock()
	if n := len(f.active); n > 0 {
		f.active = f.active[:n-1]
	}
	f.mu.Unlock()
}
