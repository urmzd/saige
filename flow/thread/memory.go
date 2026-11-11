package thread

import (
	"context"
	"fmt"
	"sync"
)

// Memory is a Store that lives in the process.
//
// It is a complete implementation of the contract, held to it by the same
// suite as Postgres: appends are atomic, contiguous and idempotent, and
// nothing it has stored ever changes. What it lacks is the one thing its name
// says, so a deployment whose conversations must survive a restart uses
// Postgres and changes nothing else.
type Memory struct {
	settings

	mu      sync.Mutex
	threads map[Key]*journal
}

// journal is one thread. The two indexes are the idempotency rule as data.
type journal struct {
	entries   []Entry
	byRun     map[runRole]int // (run id, role) -> index into entries
	bySurface map[string]int  // surface message id -> index into entries
}

type runRole struct {
	run  string
	role Role
}

// NewMemory returns an empty in-process store.
func NewMemory(opts ...Option) *Memory {
	return &Memory{settings: newSettings(opts), threads: make(map[Key]*journal)}
}

var _ Store = (*Memory)(nil)

// Append adds entries to the end of a thread. One lock covers the whole call,
// which is what makes the new entries of a call contiguous.
func (m *Memory) Append(ctx context.Context, key Key, entries ...Entry) ([]Entry, error) {
	prepared, err := m.prepare(ctx, key, entries)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	j := m.threads[key]
	if j == nil {
		if len(prepared) == 0 {
			return []Entry{}, nil
		}
		j = &journal{byRun: make(map[runRole]int), bySurface: make(map[string]int)}
		m.threads[key] = j
	}
	out := make([]Entry, len(prepared))
	for i, e := range prepared {
		if at, ok := j.held(e); ok {
			out[i] = j.entries[at].clone()
			continue
		}
		e.Seq = uint64(len(j.entries)) + 1
		j.entries = append(j.entries, e)
		j.index(e, len(j.entries)-1)
		out[i] = e.clone()
	}
	return out, nil
}

// index records where the journal holds e.
func (j *journal) index(e Entry, at int) {
	if e.RunID != "" {
		j.byRun[runRole{e.RunID, e.Role}] = at
	}
	if e.SurfaceID != "" {
		j.bySurface[e.SurfaceID] = at
	}
}

// held reports where the journal already holds e, if it does.
func (j *journal) held(e Entry) (int, bool) {
	if e.RunID != "" {
		if at, ok := j.byRun[runRole{e.RunID, e.Role}]; ok {
			return at, true
		}
	}
	if e.SurfaceID != "" {
		if at, ok := j.bySurface[e.SurfaceID]; ok {
			return at, true
		}
	}
	return 0, false
}

// Head returns the sequence of a thread's newest entry.
func (m *Memory) Head(ctx context.Context, key Key) (uint64, error) {
	if err := check(ctx, key); err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if j := m.threads[key]; j != nil {
		return uint64(len(j.entries)), nil
	}
	return 0, nil
}

// History returns the entries with Seq <= upTo, oldest first.
func (m *Memory) History(ctx context.Context, key Key, upTo uint64) ([]Entry, error) {
	if err := check(ctx, key); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var held []Entry
	if j := m.threads[key]; j != nil {
		held = j.entries
	}
	if upTo > uint64(len(held)) {
		return nil, fmt.Errorf("%w: thread %s holds %d entries, not the %d asked for", ErrGap, key, len(held), upTo)
	}
	// A copy, attachment lists included, so a caller cannot reach back into
	// the journal and edit it.
	out := make([]Entry, upTo)
	for i, e := range held[:upTo] {
		out[i] = e.clone()
	}
	return out, nil
}
