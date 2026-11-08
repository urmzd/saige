package ledger

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/urmzd/saige/durable/run"
)

// Memory is a complete single-process Ledger: every contract on the interface
// holds, guarded by one mutex. It is durable truth for programs whose lifetime
// is the process — tests, CLIs, single-node workers — not a test double.
//
// Value semantics: Memory stores and returns deep copies of every run and
// record, including their []byte fields. Callers may freely mutate anything
// they pass in or get back without corrupting the store.
//
// Records against terminal runs are allowed: a superseded attempt may still be
// checkpointing after another attempt completed the run. The write is a
// harmless zombie — the terminal result already won, and the record log stays
// append-only.
//
// ListPending matches non-terminal runs (per the Ledger contract), and treats
// limit <= 0 as "no limit".
type Memory struct {
	mu      sync.Mutex
	runs    map[string]*run.Run
	records map[string]map[string]run.Record // run ID → record key → record
}

var (
	_ Ledger       = (*Memory)(nil)
	_ RunGetter    = (*Memory)(nil)
	_ PrefixReader = (*Memory)(nil)
)

// NewMemory returns an empty in-process ledger.
func NewMemory() *Memory {
	return &Memory{
		runs:    make(map[string]*run.Run),
		records: make(map[string]map[string]run.Record),
	}
}

// Accept implements Ledger. It stores a deep copy of r; accepting an existing
// run ID is a no-op returning nil, leaving the original run untouched.
func (m *Memory) Accept(ctx context.Context, r run.Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.runs[r.ID]; ok {
		return nil
	}
	stored := cloneRun(r)
	m.runs[r.ID] = &stored
	m.records[r.ID] = make(map[string]run.Record)
	return nil
}

// Complete implements Ledger: a CAS from non-terminal to res.Status. The first
// terminal write wins; attempt is stored for audit, never checked.
func (m *Memory) Complete(ctx context.Context, runID string, attempt int, res run.Result) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	r, ok := m.runs[runID]
	if !ok {
		return fmt.Errorf("ledger: complete %q: %w", runID, run.ErrNotFound)
	}
	if r.Status.Terminal() {
		return fmt.Errorf("ledger: complete %q: %w", runID, run.ErrAlreadyTerminal)
	}
	r.Status = res.Status
	r.Output = cloneBytes(res.Output)
	r.Error = res.Error
	r.Attempt = attempt
	r.CompletedAt = time.Now()
	return nil
}

// Load implements Ledger. The returned run and records are deep copies in no
// particular order.
func (m *Memory) Load(ctx context.Context, runID string) (run.Run, []run.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	r, ok := m.runs[runID]
	if !ok {
		return run.Run{}, nil, fmt.Errorf("ledger: load %q: %w", runID, run.ErrNotFound)
	}
	recs := make([]run.Record, 0, len(m.records[runID]))
	for _, rec := range m.records[runID] {
		recs = append(recs, cloneRecord(rec))
	}
	return cloneRun(*r), recs, nil
}

// Record implements Ledger: write-once on (RunID, Key). On conflict it returns
// a copy of the already-stored record with run.ErrAlreadyRecorded — first
// write wins and the caller adopts the stored record. On first write it sets
// RecordedAt if zero and returns a copy of what was stored. Writes against a
// terminal run are allowed (see the Memory doc comment).
func (m *Memory) Record(ctx context.Context, rec run.Record) (run.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	byKey, ok := m.records[rec.RunID]
	if !ok {
		return run.Record{}, fmt.Errorf("ledger: record %q/%q: %w", rec.RunID, rec.Key, run.ErrNotFound)
	}
	if existing, ok := byKey[rec.Key]; ok {
		return cloneRecord(existing), fmt.Errorf("ledger: record %q/%q: %w", rec.RunID, rec.Key, run.ErrAlreadyRecorded)
	}
	stored := cloneRecord(rec)
	if stored.RecordedAt.IsZero() {
		stored.RecordedAt = time.Now()
	}
	byKey[rec.Key] = stored
	return cloneRecord(stored), nil
}

// GetRun implements the RunGetter capability: a deep-copied point read
// without the record prefetch.
func (m *Memory) GetRun(ctx context.Context, runID string) (run.Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	r, ok := m.runs[runID]
	if !ok {
		return run.Run{}, fmt.Errorf("ledger: get %q: %w", runID, run.ErrNotFound)
	}
	return cloneRun(*r), nil
}

// RecordsWithPrefix implements the PrefixReader capability: deep copies of
// the run's records whose key starts with one of prefixes, in no particular
// order.
func (m *Memory) RecordsWithPrefix(ctx context.Context, runID string, prefixes ...string) ([]run.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	byKey, ok := m.records[runID]
	if !ok {
		return nil, fmt.Errorf("ledger: records of %q: %w", runID, run.ErrNotFound)
	}
	var out []run.Record
	for key, rec := range byKey {
		if slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(key, p) }) {
			out = append(out, cloneRecord(rec))
		}
	}
	return out, nil
}

// ListPending implements Ledger: non-terminal runs created strictly before t,
// ascending by (CreatedAt, ID), strictly after cursor, up to limit.
// limit <= 0 means no limit.
func (m *Memory) ListPending(ctx context.Context, before time.Time, cursor run.PendingCursor, limit int) ([]run.PendingRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var pending []run.PendingRun
	for id, r := range m.runs {
		if r.Status.Terminal() || !r.CreatedAt.Before(before) {
			continue
		}
		if afterCursor(cursor, r.CreatedAt, id) {
			pending = append(pending, run.PendingRun{ID: id, CreatedAt: r.CreatedAt})
		}
	}
	slices.SortFunc(pending, func(a, b run.PendingRun) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	if limit > 0 && len(pending) > limit {
		pending = pending[:limit]
	}
	return pending, nil
}

// afterCursor reports whether (createdAt, id) sorts strictly after the
// cursor. The zero cursor admits everything.
func afterCursor(c run.PendingCursor, createdAt time.Time, id string) bool {
	if c.CreatedAt.IsZero() && c.ID == "" {
		return true
	}
	if cmpTime := createdAt.Compare(c.CreatedAt); cmpTime != 0 {
		return cmpTime > 0
	}
	return id > c.ID
}

func cloneRun(r run.Run) run.Run {
	r.Input = cloneBytes(r.Input)
	r.Output = cloneBytes(r.Output)
	return r
}

func cloneRecord(rec run.Record) run.Record {
	rec.Output = cloneBytes(rec.Output)
	return rec
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	return append([]byte(nil), b...)
}
