// Package registry provides an append-only, revisioned registry.
//
// Everything an agent deployment registers by name -- model definitions, tools,
// prompts, gates -- has the same two problems. Something overwrites an entry
// and nobody can see what it replaced, and something breaks after an update
// with no way back except a redeploy. An append-only registry solves both: a
// registration adds a revision instead of replacing one, resolution takes the
// latest by default, and a pin freezes a name to a known-good revision until
// it is explicitly released.
//
// The cost of a revision is one slice append, so history is kept for
// everything rather than only for the entries someone predicted would need it.
package registry

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Revision numbers an entry's versions, starting at 1 and increasing by one per
// registration of that name. Revisions are per-name, not global, so "model X
// revision 2" reads as the second definition of that model.
type Revision int

// Entry is one revision of one name.
type Entry[T any] struct {
	Name     string
	Revision Revision
	Value    T
	// RecordedAt is when the revision was registered. Set from the clock unless
	// the caller supplied one, so history is orderable even when revisions are
	// registered concurrently.
	RecordedAt time.Time
	// Source names what registered it: a package init, a config file path, an
	// operator. An entry whose provenance is unknown is one nobody can judge.
	Source string
	// Note is a free-form reason for the change.
	Note string
}

// Option configures a registration.
type Option func(*entryMeta)

type entryMeta struct {
	source string
	note   string
	at     time.Time
}

// WithSource records what registered the revision.
func WithSource(source string) Option {
	return func(m *entryMeta) { m.source = source }
}

// WithNote records why.
func WithNote(note string) Option {
	return func(m *entryMeta) { m.note = note }
}

// WithTime overrides the recorded timestamp, for deterministic tests.
func WithTime(t time.Time) Option {
	return func(m *entryMeta) { m.at = t }
}

// Registry holds revisions by name. Safe for concurrent use.
type Registry[T any] struct {
	mu      sync.RWMutex
	history map[string][]Entry[T]
	pins    map[string]Revision
}

// New returns an empty registry.
func New[T any]() *Registry[T] {
	return &Registry[T]{
		history: map[string][]Entry[T]{},
		pins:    map[string]Revision{},
	}
}

// Register appends a new revision for name and returns it. It never overwrites:
// the previous revision stays resolvable, which is what makes Rollback possible
// without re-registering the old value from memory.
func (r *Registry[T]) Register(name string, value T, opts ...Option) Entry[T] {
	m := entryMeta{}
	for _, o := range opts {
		o(&m)
	}
	if m.at.IsZero() {
		m.at = time.Now()
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	rev := Revision(len(r.history[name]) + 1)
	e := Entry[T]{
		Name:       name,
		Revision:   rev,
		Value:      value,
		RecordedAt: m.at,
		Source:     m.source,
		Note:       m.note,
	}
	r.history[name] = append(r.history[name], e)
	return e
}

// Get resolves a name to the value that should be used now: the pinned revision
// when one is set, otherwise the latest.
func (r *Registry[T]) Get(name string) (T, bool) {
	e, ok := r.Resolve(name)
	return e.Value, ok
}

// Resolve is Get with the full entry, so a caller can log which revision it
// actually used. Worth doing at startup: "loaded model X rev 3 (pinned)" turns
// a later surprise into a one-line diagnosis.
func (r *Registry[T]) Resolve(name string) (Entry[T], bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	revs := r.history[name]
	if len(revs) == 0 {
		var zero Entry[T]
		return zero, false
	}
	if pin, ok := r.pins[name]; ok {
		if pin >= 1 && int(pin) <= len(revs) {
			return revs[pin-1], true
		}
	}
	return revs[len(revs)-1], true
}

// At returns a specific revision.
func (r *Registry[T]) At(name string, rev Revision) (Entry[T], bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	revs := r.history[name]
	if rev < 1 || int(rev) > len(revs) {
		var zero Entry[T]
		return zero, false
	}
	return revs[rev-1], true
}

// Latest returns the newest revision, ignoring any pin.
func (r *Registry[T]) Latest(name string) (Entry[T], bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	revs := r.history[name]
	if len(revs) == 0 {
		var zero Entry[T]
		return zero, false
	}
	return revs[len(revs)-1], true
}

// History returns every revision of a name, oldest first.
func (r *Registry[T]) History(name string) []Entry[T] {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Entry[T], len(r.history[name]))
	copy(out, r.history[name])
	return out
}

// Pin freezes a name to a revision. Resolution returns that revision until
// Unpin, including for revisions registered afterwards: a pin is a statement
// that newer is not automatically better.
func (r *Registry[T]) Pin(name string, rev Revision) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	revs := r.history[name]
	if len(revs) == 0 {
		return fmt.Errorf("registry: cannot pin unknown name %q", name)
	}
	if rev < 1 || int(rev) > len(revs) {
		return fmt.Errorf("registry: %q has no revision %d (have 1..%d)", name, rev, len(revs))
	}
	r.pins[name] = rev
	return nil
}

// Unpin releases a pin, returning the name to latest-wins resolution.
func (r *Registry[T]) Unpin(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.pins, name)
}

// Pinned reports the pinned revision, if any.
func (r *Registry[T]) Pinned(name string) (Revision, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rev, ok := r.pins[name]
	return rev, ok
}

// Rollback pins a name to the revision before the one currently resolving, and
// returns the entry now in effect. It is the operational verb: something broke
// after an update, go back one, investigate later.
func (r *Registry[T]) Rollback(name string) (Entry[T], error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	revs := r.history[name]
	var zero Entry[T]
	if len(revs) == 0 {
		return zero, fmt.Errorf("registry: cannot roll back unknown name %q", name)
	}
	current := Revision(len(revs))
	if pin, ok := r.pins[name]; ok {
		current = pin
	}
	if current <= 1 {
		return zero, fmt.Errorf("registry: %q is already at revision 1", name)
	}
	r.pins[name] = current - 1
	return revs[current-2], nil
}

// Names returns every registered name, sorted.
func (r *Registry[T]) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.history))
	for n := range r.history {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// All returns the currently-resolving entry for every name, sorted by name.
func (r *Registry[T]) All() []Entry[T] {
	names := r.Names()
	out := make([]Entry[T], 0, len(names))
	for _, n := range names {
		if e, ok := r.Resolve(n); ok {
			out = append(out, e)
		}
	}
	return out
}

// Len returns the number of distinct names.
func (r *Registry[T]) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.history)
}
