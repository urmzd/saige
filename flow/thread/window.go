package thread

import (
	"context"
	"fmt"
)

const MaxWindow = 200

// WindowStore is the bounded read seam used by session APIs. It does not alter
// the history pin contract: a replay still reads exactly its pinned prefix.
type WindowStore interface {
	Window(context.Context, Key, uint64, int) ([]Entry, error)
}

func ReadWindow(ctx context.Context, s Store, k Key, since uint64, n int) ([]Entry, error) {
	if n <= 0 || n > MaxWindow {
		n = MaxWindow
	}
	if bounded, ok := s.(WindowStore); ok {
		return bounded.Window(ctx, k, since, n)
	}
	head, err := s.Head(ctx, k)
	if err != nil {
		return nil, err
	}
	if head > MaxWindow {
		return nil, fmt.Errorf("%w: store must implement bounded Window for session reads", ErrInvalid)
	}
	all, err := s.History(ctx, k, head)
	if err != nil {
		return nil, err
	}
	out := []Entry{}
	for _, e := range all {
		if e.Seq > since {
			out = append(out, e)
			if len(out) == n {
				break
			}
		}
	}
	return out, nil
}
func (p *Postgres) Window(ctx context.Context, k Key, since uint64, n int) ([]Entry, error) {
	if err := check(ctx, k); err != nil {
		return nil, err
	}
	if n <= 0 || n > MaxWindow {
		n = MaxWindow
	}
	// A cursor beyond the signed database range cannot have a following entry.
	if since > 1<<63-1 {
		return []Entry{}, nil
	}
	rows, err := p.pool.Query(ctx, `SELECT `+entryColumns+` FROM fiber_thread_entries WHERE tenant=$1 AND agent=$2 AND surface=$3 AND seq>$4 ORDER BY seq LIMIT $5`, k.Tenant, k.Agent, k.Surface, int64(since), n)
	if err != nil {
		return nil, err
	}
	return collect(rows)
}
func (m *Memory) Window(ctx context.Context, k Key, since uint64, n int) ([]Entry, error) {
	if err := check(ctx, k); err != nil {
		return nil, err
	}
	if n <= 0 || n > MaxWindow {
		n = MaxWindow
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Entry{}
	j := m.threads[k]
	if j == nil || since >= uint64(len(j.entries)) {
		return out, nil
	}
	start := int(since) //nolint:gosec // a window start is bounded by the journal length
	for _, e := range j.entries[start:min(len(j.entries), start+n)] {
		out = append(out, e.clone())
	}
	return out, nil
}
