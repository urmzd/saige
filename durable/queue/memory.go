package queue

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/urmzd/saige/durable/run"
)

// memState is a run's position in the queue lifecycle.
type memState int

const (
	// memSettled is the tombstone state: the run is not deliverable, but
	// its attempt and failure counters survive so a later Enqueue continues
	// the lineage. This is load-bearing for fencing — a parked run that is
	// settled and re-enqueued must claim at attempt N+1, never reset to 1,
	// or a zombie holding attempt N would pass the fence.
	memSettled memState = iota
	// memQueued means claimable once readyAt passes.
	memQueued
	// memLeased means claimed under a lease; reclaimable after deadline.
	memLeased
)

// memEntry is the per-run queue state. Entries are never deleted: Settle
// leaves a memSettled tombstone carrying the counters.
type memEntry struct {
	state    memState
	readyAt  time.Time // valid when state == memQueued
	deadline time.Time // valid when state == memLeased
	attempt  int       // fencing: monotonic across release, expiry, and settle
	failures int       // retry budget: expiry reclaims and failed releases only
}

// memLog is a run's delta history plus its trim epoch. Trim bumps the epoch
// so cursors and streams from before the trim fail loudly instead of
// stalling on indexes into a log that no longer exists.
type memLog struct {
	epoch  int
	deltas []run.Delta
}

// Memory is a complete single-process Queue and DeltaLog guarded by one
// mutex. Like every queue backend it is disposable flow, not truth — but
// every contract holds: fenced leases, exclusive claims, expiry reclaim,
// attempt lineage that survives Settle, and a failure counter that moves
// only when an execution actually failed (expiry, or Release with
// failed=true) — never when a parked run settles and resumes.
//
// Blocked Claim and Recv callers wait on a broadcast channel that is closed
// and replaced on every relevant state change, plus a timer aimed at the
// earliest readyAt or lease deadline — no busy-spinning, and millisecond-
// scale TTLs wake claimers promptly.
//
// Deltas are value-semantic like ledger.Memory: payloads are deep-copied on
// Append and again on the way out of Read and Recv.
type Memory struct {
	mu      sync.Mutex
	entries map[string]*memEntry
	logs    map[string]*memLog

	// queueWake is closed and replaced whenever queue state changes in a
	// way that could unblock a Claim. Claimers grab the current channel
	// under the lock, so a change after they unlock still wakes them.
	queueWake chan struct{}
	// logWake is the same broadcast for delta appends, waking Recv.
	logWake chan struct{}
}

var (
	_ Queue    = (*Memory)(nil)
	_ DeltaLog = (*Memory)(nil)
)

// NewMemory returns an empty in-process queue and delta log.
func NewMemory() *Memory {
	return &Memory{
		entries:   make(map[string]*memEntry),
		logs:      make(map[string]*memLog),
		queueWake: make(chan struct{}),
		logWake:   make(chan struct{}),
	}
}

// Enqueue implements Queue. A run that is already queued or leased is left
// untouched; a settled (or unknown) run becomes claimable at now+delay,
// keeping whatever counters it had.
func (m *Memory) Enqueue(ctx context.Context, runID string, delay time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.entries[runID]
	if !ok {
		e = &memEntry{}
		m.entries[runID] = e
	} else if e.state != memSettled {
		return nil // already queued or leased: idempotent no-op
	}
	e.state = memQueued
	e.readyAt = time.Now().Add(delay)
	m.wakeQueueLocked()
	return nil
}

// Claim implements Queue. It hands each claimable run — queued past readyAt,
// or leased past its deadline — to exactly one caller, incrementing the
// attempt. Reclaiming an expired lease also increments the failure counter:
// the previous execution died holding it. When nothing is due it sleeps
// until the earliest upcoming readyAt/deadline or a wake broadcast.
func (m *Memory) Claim(ctx context.Context, ttl time.Duration) (Item, time.Time, error) {
	for {
		if err := ctx.Err(); err != nil {
			return Item{}, time.Time{}, err
		}

		m.mu.Lock()
		now := time.Now()
		var (
			pickID   string
			pick     *memEntry
			expired  bool
			nextWake time.Time
		)
		for id, e := range m.entries {
			var due time.Time
			switch e.state {
			case memQueued:
				due = e.readyAt
			case memLeased:
				due = e.deadline
			default:
				continue
			}
			if !due.After(now) {
				pickID, pick, expired = id, e, e.state == memLeased
				break
			}
			if nextWake.IsZero() || due.Before(nextWake) {
				nextWake = due
			}
		}
		if pick != nil {
			pick.attempt++
			if expired {
				pick.failures++
			}
			pick.state = memLeased
			pick.deadline = now.Add(ttl)
			it := Item{RunID: pickID, Attempt: pick.attempt, Failures: pick.failures}
			deadline := pick.deadline
			m.mu.Unlock()
			return it, deadline, nil
		}
		wake := m.queueWake
		m.mu.Unlock()

		var (
			timer  *time.Timer
			timerC <-chan time.Time
		)
		if !nextWake.IsZero() {
			timer = time.NewTimer(time.Until(nextWake))
			timerC = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return Item{}, time.Time{}, ctx.Err()
		case <-wake:
		case <-timerC:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// Heartbeat implements Queue. The lease must still be held by it.Attempt;
// anything else — released, settled, reclaimed — gets run.ErrSuperseded.
func (m *Memory) Heartbeat(ctx context.Context, it Item, ttl time.Duration) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.entries[it.RunID]
	if !ok || e.state != memLeased || e.attempt != it.Attempt {
		return time.Time{}, fmt.Errorf("queue: heartbeat %q attempt %d: %w", it.RunID, it.Attempt, run.ErrSuperseded)
	}
	e.deadline = time.Now().Add(ttl)
	return e.deadline, nil
}

// Release implements Queue. Fenced by it.Attempt: the run goes back to
// queued, claimable at now+delay. failed=true consumes retry budget;
// failed=false (interruption) does not.
func (m *Memory) Release(ctx context.Context, it Item, delay time.Duration, failed bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.entries[it.RunID]
	if !ok || e.state != memLeased || e.attempt != it.Attempt {
		return fmt.Errorf("queue: release %q attempt %d: %w", it.RunID, it.Attempt, run.ErrSuperseded)
	}
	if failed {
		e.failures++
	}
	e.state = memQueued
	e.readyAt = time.Now().Add(delay)
	m.wakeQueueLocked()
	return nil
}

// Settle implements Queue. Fenced by it.Attempt: the run leaves delivery but
// its counters are kept as a tombstone, so a later Enqueue (a parked run
// resuming) continues the lineage — at attempt N+1 and with the failure
// budget untouched. The run's delta log is separate; see Trim.
func (m *Memory) Settle(ctx context.Context, it Item) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.entries[it.RunID]
	if !ok || e.state != memLeased || e.attempt != it.Attempt {
		return fmt.Errorf("queue: settle %q attempt %d: %w", it.RunID, it.Attempt, run.ErrSuperseded)
	}
	e.state = memSettled
	return nil
}

// wakeQueueLocked broadcasts to every blocked Claim. Callers hold m.mu.
func (m *Memory) wakeQueueLocked() {
	close(m.queueWake)
	m.queueWake = make(chan struct{})
}

// Append implements DeltaLog. A delta carrying an attempt below the run's
// current counter is a zombie and gets run.ErrSuperseded; appends for
// unknown runs, and for the current attempt even when the run is not
// leased, are allowed. The payload is copied.
func (m *Memory) Append(ctx context.Context, d run.Delta) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if e, ok := m.entries[d.RunID]; ok && d.Attempt < e.attempt {
		return fmt.Errorf("queue: append %q attempt %d: %w", d.RunID, d.Attempt, run.ErrSuperseded)
	}
	d.Payload = cloneBytes(d.Payload)
	l := m.logs[d.RunID]
	if l == nil {
		l = &memLog{}
		m.logs[d.RunID] = l
	}
	l.deltas = append(l.deltas, d)
	close(m.logWake)
	m.logWake = make(chan struct{})
	return nil
}

// Watch implements DeltaLog. The returned Stream replays history from the
// cursor, then blocks for live appends. Every watcher reads the shared log
// at its own position, so concurrent watchers each see every delta. A
// stream whose cursor predates a Trim fails with an invalid-cursor error
// rather than stalling.
func (m *Memory) Watch(ctx context.Context, runID string, from Cursor) (Stream, error) {
	m.mu.Lock()
	epoch, next, err := m.resolveCursorLocked(runID, from)
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &memoryStream{m: m, runID: runID, epoch: epoch, next: next}, nil
}

// Read implements DeltaLog: a copied snapshot from the cursor, at most limit
// deltas (limit <= 0 means no bound), and the cursor one past the last delta
// returned. Memory's cursor is "epoch.index"; "" is the start of the current
// epoch, and a cursor from before a Trim errors instead of misreading.
func (m *Memory) Read(ctx context.Context, runID string, from Cursor, limit int) ([]run.Delta, Cursor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	epoch, start, err := m.resolveCursorLocked(runID, from)
	if err != nil {
		return nil, "", err
	}
	var log []run.Delta
	if l := m.logs[runID]; l != nil {
		log = l.deltas
	}
	if start > len(log) {
		start = len(log)
	}
	rest := log[start:]
	if limit > 0 && limit < len(rest) {
		rest = rest[:limit]
	}
	out := make([]run.Delta, len(rest))
	for i, d := range rest {
		d.Payload = cloneBytes(d.Payload)
		out[i] = d
	}
	return out, formatCursor(epoch, start+len(out)), nil
}

// Trim implements DeltaLog: it drops the run's deltas and bumps the log's
// epoch, invalidating outstanding cursors and streams. Queue state and the
// counters are untouched.
func (m *Memory) Trim(ctx context.Context, runID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	l := m.logs[runID]
	if l == nil {
		l = &memLog{}
		m.logs[runID] = l
	}
	l.epoch++
	l.deltas = nil
	// Wake blocked streams so stale watchers fail now, not never.
	close(m.logWake)
	m.logWake = make(chan struct{})
	return nil
}

// resolveCursorLocked decodes a cursor against the run's current epoch.
// Callers hold m.mu.
func (m *Memory) resolveCursorLocked(runID string, c Cursor) (epoch, index int, err error) {
	current := 0
	if l := m.logs[runID]; l != nil {
		current = l.epoch
	}
	if c == "" {
		return current, 0, nil
	}
	e, i, ok := strings.Cut(string(c), ".")
	if !ok {
		return 0, 0, fmt.Errorf("queue: invalid cursor %q", c)
	}
	epoch, err1 := strconv.Atoi(e)
	index, err2 := strconv.Atoi(i)
	if err1 != nil || err2 != nil || epoch < 0 || index < 0 {
		return 0, 0, fmt.Errorf("queue: invalid cursor %q", c)
	}
	if epoch != current {
		return 0, 0, fmt.Errorf("queue: cursor %q invalidated by trim", c)
	}
	return epoch, index, nil
}

func formatCursor(epoch, index int) Cursor {
	return Cursor(strconv.Itoa(epoch) + "." + strconv.Itoa(index))
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	return append([]byte(nil), b...)
}

// memoryStream is a Watch handle: an index into the shared per-run log,
// pinned to the epoch it was created under.
type memoryStream struct {
	m     *Memory
	runID string
	epoch int
	next  int
}

// Recv implements Stream. It returns the next recorded delta immediately, or
// blocks on the append broadcast until one arrives or ctx is done. If the
// log was trimmed under the stream, Recv fails with an invalid-cursor error.
func (s *memoryStream) Recv(ctx context.Context) (run.Delta, Cursor, error) {
	for {
		if err := ctx.Err(); err != nil {
			return run.Delta{}, "", err
		}

		s.m.mu.Lock()
		l := s.m.logs[s.runID]
		epoch := 0
		var log []run.Delta
		if l != nil {
			epoch, log = l.epoch, l.deltas
		}
		if epoch != s.epoch {
			s.m.mu.Unlock()
			return run.Delta{}, "", fmt.Errorf("queue: stream at epoch %d invalidated by trim", s.epoch)
		}
		if s.next < len(log) {
			d := log[s.next]
			d.Payload = cloneBytes(d.Payload)
			s.next++
			cur := formatCursor(s.epoch, s.next)
			s.m.mu.Unlock()
			return d, cur, nil
		}
		wake := s.m.logWake
		s.m.mu.Unlock()

		select {
		case <-ctx.Done():
			return run.Delta{}, "", ctx.Err()
		case <-wake:
		}
	}
}
