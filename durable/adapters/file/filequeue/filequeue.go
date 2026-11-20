// Package filequeue implements durable's queue.Queue on a local directory,
// beside fileledger: each run's entry is queue.json in the run's directory
// (fileledger.RunDir), replaced atomically under queue.lock.
//
// Leases are time leases with the same semantics as queue.Memory. A claimer
// also holds an advisory lock on the run's lease.lock for as long as it
// holds the lease, and a lease whose lock is free is treated as lapsed: a
// crashed process gives its runs back at once instead of after the TTL. The
// lock is only that signal; the deadline and the attempt still fence every
// call.
//
// Claim scans every entry, so the queue suits the runs of one machine, not a
// fleet. Another process's Enqueue is seen within the poll interval.
package filequeue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/urmzd/saige/durable/adapters/file/fileledger"
	"github.com/urmzd/saige/durable/adapters/file/internal/fsutil"
	"github.com/urmzd/saige/durable/queue"
	"github.com/urmzd/saige/durable/run"
)

const (
	entryFile = "queue.json"
	entryLock = "queue.lock"
	leaseLock = "lease.lock"

	// poll is how often a waiting Claim looks for work another process
	// enqueued. Enqueues in this process wake it at once.
	poll = 100 * time.Millisecond
)

type state string

const (
	settled state = "settled"
	queued  state = "queued"
	leased  state = "leased"
)

type entry struct {
	RunID    string    `json:"run_id"`
	State    state     `json:"state"`
	ReadyAt  time.Time `json:"ready_at,omitzero"`
	Deadline time.Time `json:"deadline,omitzero"`
	Attempt  int       `json:"attempt"`
	Failures int       `json:"failures"`
	// Locked says the holder holds lease.lock, so a free lock means it died.
	// A lease taken while a lapsed holder in the same process still had the
	// lock is not Locked, and only its deadline decides.
	Locked bool `json:"locked,omitempty"`
}

// Queue is a queue.Queue on files under Dir.
type Queue struct {
	dir string

	mu    sync.Mutex
	held  map[string]func() // run ID + attempt -> release of its lease lock
	wake  chan struct{}
	clock func() time.Time
}

var (
	_ queue.Queue       = (*Queue)(nil)
	_ queue.Targeted    = (*Queue)(nil)
	_ queue.LeaseProber = (*Queue)(nil)
)

// New returns a queue on dir, which it creates with mode 0700 if needed. Use
// the directory of the fileledger the runs are in.
func New(dir string) (*Queue, error) {
	if dir == "" {
		return nil, errors.New("filequeue: directory required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("filequeue: %w", err)
	}
	return &Queue{dir: dir, held: map[string]func(){}, wake: make(chan struct{}), clock: time.Now}, nil
}

func (q *Queue) runDir(id string) string { return fileledger.RunDir(q.dir, id) }

// Enqueue implements queue.Queue.
func (q *Queue) Enqueue(ctx context.Context, runID string, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := q.update(runID, true, func(e *entry) error {
		if e.State == queued || e.State == leased {
			return errNoChange
		}
		e.State, e.ReadyAt = queued, q.clock().Add(delay)
		return nil
	})
	if err != nil && !errors.Is(err, errNoChange) {
		return fmt.Errorf("filequeue: enqueue %q: %w", runID, err)
	}
	q.signal()
	return nil
}

// Claim implements queue.Queue.
func (q *Queue) Claim(ctx context.Context, ttl time.Duration) (queue.Item, time.Time, error) {
	for {
		if err := ctx.Err(); err != nil {
			return queue.Item{}, time.Time{}, err
		}
		q.mu.Lock()
		wake := q.wake
		q.mu.Unlock()
		ids, next, err := q.due()
		if err != nil {
			return queue.Item{}, time.Time{}, err
		}
		for _, id := range ids {
			it, deadline, err := q.take(id, ttl, false)
			if err == nil {
				return it, deadline, nil
			}
			if !errors.Is(err, errNoChange) {
				return queue.Item{}, time.Time{}, err
			}
		}
		wait := poll
		if !next.IsZero() {
			wait = min(wait, max(time.Until(next), time.Millisecond))
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return queue.Item{}, time.Time{}, ctx.Err()
		case <-wake:
		case <-timer.C:
		}
		timer.Stop()
	}
}

// ClaimRun implements queue.Targeted: it leases runID now, whatever its
// delay. A run another holder leases is queue.ErrLeased; a run that is not
// queued is queue.ErrNotQueued.
func (q *Queue) ClaimRun(ctx context.Context, runID string, ttl time.Duration) (queue.Item, time.Time, error) {
	if err := ctx.Err(); err != nil {
		return queue.Item{}, time.Time{}, err
	}
	it, deadline, err := q.take(runID, ttl, true)
	if errors.Is(err, errNoChange) {
		return queue.Item{}, time.Time{}, fmt.Errorf("filequeue: claim %q: %w", runID, queue.ErrNotQueued)
	}
	return it, deadline, err
}

// Leased implements queue.LeaseProber: whether a live holder leases runID.
func (q *Queue) Leased(ctx context.Context, runID string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	e, err := q.read(runID)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return q.live(e), nil
}

// Heartbeat implements queue.Queue.
func (q *Queue) Heartbeat(ctx context.Context, it queue.Item, ttl time.Duration) (time.Time, error) {
	var deadline time.Time
	err := q.update(it.RunID, false, func(e *entry) error {
		if e.State != leased || e.Attempt != it.Attempt {
			return run.ErrSuperseded
		}
		e.Deadline = q.clock().Add(ttl)
		deadline = e.Deadline
		return nil
	})
	if err != nil {
		q.drop(it)
		return time.Time{}, fmt.Errorf("filequeue: heartbeat %q attempt %d: %w", it.RunID, it.Attempt, superseded(err))
	}
	return deadline, nil
}

// Release implements queue.Queue.
func (q *Queue) Release(ctx context.Context, it queue.Item, delay time.Duration, failed bool) error {
	defer q.drop(it)
	err := q.update(it.RunID, false, func(e *entry) error {
		if e.State != leased || e.Attempt != it.Attempt {
			return run.ErrSuperseded
		}
		if failed {
			e.Failures++
		}
		e.State, e.ReadyAt, e.Deadline, e.Locked = queued, q.clock().Add(delay), time.Time{}, false
		return nil
	})
	if err != nil {
		return fmt.Errorf("filequeue: release %q attempt %d: %w", it.RunID, it.Attempt, superseded(err))
	}
	q.signal()
	return nil
}

// Settle implements queue.Queue.
func (q *Queue) Settle(ctx context.Context, it queue.Item) error {
	defer q.drop(it)
	err := q.update(it.RunID, false, func(e *entry) error {
		if e.State != leased || e.Attempt != it.Attempt {
			return run.ErrSuperseded
		}
		e.State, e.Deadline, e.Locked = settled, time.Time{}, false
		return nil
	})
	if err != nil {
		return fmt.Errorf("filequeue: settle %q attempt %d: %w", it.RunID, it.Attempt, superseded(err))
	}
	return nil
}

// Remove deletes runID's entry and lock files, for a host deleting the run.
func (q *Queue) Remove(runID string) error {
	dir := q.runDir(runID)
	err := errors.Join(ignoreMissing(os.Remove(filepath.Join(dir, entryFile))),
		ignoreMissing(os.Remove(filepath.Join(dir, entryLock))), ignoreMissing(os.Remove(filepath.Join(dir, leaseLock))))
	_ = os.Remove(dir) // only when empty
	return err
}

var errNoChange = errors.New("filequeue: no change")

// take leases runID if it is claimable: queued and due (or any queued run
// when targeted), or leased by a holder that lapsed or died.
func (q *Queue) take(runID string, ttl time.Duration, targeted bool) (queue.Item, time.Time, error) {
	var it queue.Item
	var deadline time.Time
	err := q.update(runID, false, func(e *entry) error {
		now := q.clock()
		switch e.State {
		case queued:
			if !targeted && e.ReadyAt.After(now) {
				return errNoChange
			}
		case leased:
			if q.live(*e) {
				if targeted {
					return queue.ErrLeased
				}
				return errNoChange
			}
			e.Failures++ // the previous execution lapsed or died
		default:
			return errNoChange
		}
		e.Attempt++
		e.State, e.Deadline, e.ReadyAt = leased, now.Add(ttl), time.Time{}
		it = queue.Item{RunID: runID, Attempt: e.Attempt, Failures: e.Failures}
		deadline = e.Deadline
		// Take the lease lock while the entry is locked, so no other
		// claimer sees the new lease without it.
		release, lerr := fsutil.TryLock(filepath.Join(q.runDir(runID), leaseLock))
		e.Locked = lerr == nil
		if e.Locked {
			q.mu.Lock()
			q.held[heldKey(it)] = release
			q.mu.Unlock()
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return queue.Item{}, time.Time{}, errNoChange
	}
	if errors.Is(err, queue.ErrLeased) {
		return queue.Item{}, time.Time{}, fmt.Errorf("filequeue: claim %q: %w", runID, queue.ErrLeased)
	}
	if err != nil {
		q.drop(it)
		return queue.Item{}, time.Time{}, err
	}
	return it, deadline, nil
}

// live reports whether e is a lease a holder still has: before its deadline,
// with its lease lock held.
func (q *Queue) live(e entry) bool {
	if e.State != leased || !e.Deadline.After(q.clock()) {
		return false
	}
	if !e.Locked {
		return true
	}
	release, err := fsutil.TryLock(filepath.Join(q.runDir(e.RunID), leaseLock))
	if err != nil {
		return true // held, or unknowable: the deadline decides
	}
	release()
	return false
}

// due lists the runs a Claim could take now, and the earliest time a
// waiting one becomes due.
func (q *Queue) due() ([]string, time.Time, error) {
	entries, err := os.ReadDir(q.dir)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("filequeue: scan: %w", err)
	}
	now := q.clock()
	var ids []string
	var next time.Time
	for _, de := range entries {
		if !de.IsDir() {
			continue
		}
		e, err := readEntry(filepath.Join(q.dir, de.Name()))
		if err != nil {
			continue
		}
		var at time.Time
		switch e.State {
		case queued:
			at = e.ReadyAt
		case leased:
			at = e.Deadline
			if !q.live(e) {
				at = now
			}
		default:
			continue
		}
		if !at.After(now) {
			ids = append(ids, e.RunID)
		} else if next.IsZero() || at.Before(next) {
			next = at
		}
	}
	return ids, next, nil
}

func (q *Queue) read(runID string) (entry, error) { return readEntry(q.runDir(runID)) }

// update applies fn to runID's entry under its lock. create starts a missing
// entry as settled; otherwise a missing entry is os.ErrNotExist.
func (q *Queue) update(runID string, create bool, fn func(*entry) error) error {
	dir := q.runDir(runID)
	if create {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	} else if _, err := os.Stat(filepath.Join(dir, entryFile)); err != nil {
		return err
	}
	unlock, err := fsutil.Lock(filepath.Join(dir, entryLock))
	if err != nil {
		return err
	}
	defer unlock()
	e, err := readEntry(dir)
	switch {
	case errors.Is(err, os.ErrNotExist) && create:
		e = entry{RunID: runID, State: settled}
	case err != nil:
		return err
	}
	if err := fn(&e); err != nil {
		return err
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return fsutil.Replace(dir, entryFile, raw)
}

func readEntry(dir string) (entry, error) {
	var e entry
	raw, err := os.ReadFile(filepath.Join(dir, entryFile)) // #nosec G304 -- Dir plus a SHA-256 run ID.
	if err != nil {
		return e, err
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		return e, fmt.Errorf("decode %s: %w", entryFile, err)
	}
	return e, nil
}

func (q *Queue) signal() {
	q.mu.Lock()
	close(q.wake)
	q.wake = make(chan struct{})
	q.mu.Unlock()
}

// drop releases the lease lock this process holds for it, if any.
func (q *Queue) drop(it queue.Item) {
	q.mu.Lock()
	release, ok := q.held[heldKey(it)]
	delete(q.held, heldKey(it))
	q.mu.Unlock()
	if ok {
		release()
	}
}

func heldKey(it queue.Item) string { return fmt.Sprintf("%s\x00%d", it.RunID, it.Attempt) }

func superseded(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return run.ErrSuperseded
	}
	return err
}

func ignoreMissing(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
