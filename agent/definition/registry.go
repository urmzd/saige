package definition

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Checks validate the references a definition makes outside the set of
// definitions. A nil check skips that kind of reference.
type Checks struct {
	// Model reports whether a model reference (a preset name or
	// provider/model) can be resolved, for example against a catalog.
	Model func(ref string) error
	// Skill reports whether a skill exists. hash is the pinned hash, or
	// empty.
	Skill func(name, hash string) error
}

// Registry resolves references to definitions. It loads its source as a
// whole and validates every reference before it replaces what it holds, so
// a reload that breaks a reference keeps the previous set.
//
// A resolution is pinned: Resolve returns an immutable *Resolved, whose
// Digest covers the definition and every sub-agent it resolved to. A
// reload never changes a Resolved already handed out, so a running agent
// keeps the definition it started with, and Pinned finds it again by
// digest.
type Registry struct {
	src    Source
	checks Checks

	reload sync.Mutex // serializes Load

	mu       sync.RWMutex
	byName   map[string][]*Definition // highest version first
	all      []*Definition
	revision int
	resolved map[string]*Resolved // by name@version, for this revision
	pinned   map[string]*Resolved // by digest, across revisions
}

// Resolved is a definition with its sub-agents resolved, pinned by Digest.
type Resolved struct {
	*Definition
	// Digest is "sha256:<hex>" over the definition's digest and each
	// sub-agent's resolved digest, so a change anywhere in the tree
	// changes it.
	Digest    string
	Subagents []ResolvedSubagent
}

// ResolvedSubagent is one sub-agent reference and what it resolved to.
type ResolvedSubagent struct {
	SubagentRef
	Agent *Resolved
}

// NewRegistry returns a registry over src. Call Load before Resolve.
func NewRegistry(src Source, checks Checks) *Registry {
	return &Registry{src: src, checks: checks, pinned: map[string]*Resolved{}}
}

// Load loads the source, validates every definition and reference, and
// replaces the registry's contents. changed reports whether any definition
// was added, removed or edited. On error the previous contents stay.
func (r *Registry) Load(ctx context.Context) (changed bool, err error) {
	r.reload.Lock()
	defer r.reload.Unlock()
	if r.src == nil {
		return false, errors.New("agent definitions: nil source")
	}
	defs, err := r.src.Load(ctx)
	if err != nil {
		return false, err
	}
	byName, err := index(defs)
	if err != nil {
		return false, err
	}
	if err := r.validate(byName); err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	changed = !sameDigests(r.all, defs)
	if changed || r.byName == nil {
		r.byName, r.all = byName, defs
		r.revision++
		r.resolved = map[string]*Resolved{}
	}
	return changed, nil
}

func sameDigests(a, b []*Definition) bool {
	key := func(list []*Definition) []string {
		out := make([]string, len(list))
		for i, d := range list {
			out[i] = d.ID() + "\x00" + d.Digest
		}
		slices.Sort(out)
		return out
	}
	return slices.Equal(key(a), key(b))
}

// index groups definitions by name, highest version first, and rejects
// duplicates.
func index(defs []*Definition) (map[string][]*Definition, error) {
	byName := map[string][]*Definition{}
	seen := map[string]*Definition{}
	for _, d := range defs {
		if prev, dup := seen[d.ID()]; dup {
			return nil, &ValidationError{Issues: []Issue{{Code: CodeDuplicate,
				Message: fmt.Sprintf("%s is defined twice: %s and %s", d.ID(), prev.Location(), d.Location())}}}
		}
		seen[d.ID()] = d
		byName[d.Name] = append(byName[d.Name], d)
	}
	for _, list := range byName {
		sort.SliceStable(list, func(i, j int) bool {
			vi, _ := ParseVersion(list[i].Version)
			vj, _ := ParseVersion(list[j].Version)
			return vi.Compare(vj) > 0
		})
	}
	return byName, nil
}

// pick returns the highest version of name the range admits.
func pick(byName map[string][]*Definition, ref Ref) (*Definition, error) {
	rng, err := ParseRange(ref.Range)
	if err != nil {
		return nil, err
	}
	list := byName[ref.Name]
	for _, d := range list {
		v, err := ParseVersion(d.Version)
		if err == nil && rng.Matches(v) {
			return d, nil
		}
	}
	if len(list) == 0 {
		names := make([]string, 0, len(byName))
		for n := range byName {
			names = append(names, n)
		}
		hint := ""
		if s := suggest(ref.Name, names); s != "" {
			hint = fmt.Sprintf("; did you mean %q?", s)
		}
		return nil, fmt.Errorf("%w: no definition named %q%s", ErrNotFound, ref.Name, hint)
	}
	versions := make([]string, len(list))
	for i, d := range list {
		versions[i] = d.Version
	}
	return nil, fmt.Errorf("%w: no version of %s matches %q (available: %s)", ErrNotFound, ref.Name, ref.Range, strings.Join(versions, ", "))
}

// validate checks every reference of every definition and rejects cycles.
func (r *Registry) validate(byName map[string][]*Definition) error {
	var errs []error
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		for _, d := range byName[n] {
			var found issues
			for i, s := range d.Subagents {
				ref, err := ParseRef(s.Ref)
				if err != nil {
					continue // Parse already reported it
				}
				if _, err := pick(byName, ref); err != nil {
					found.add(fmt.Sprintf("subagents[%d].ref", i), 0, CodeUnresolved, "%v", unwrapNotFound(err))
				}
			}
			if r.checks.Model != nil && d.Model != nil {
				for i, ref := range append([]string{d.Model.Use}, d.Model.Fallback...) {
					path := "model.use"
					if i > 0 {
						path = fmt.Sprintf("model.fallback[%d]", i-1)
					}
					if err := r.checks.Model(ref); err != nil {
						found.add(path, 0, CodeUnresolved, "%v", err)
					}
				}
			}
			if r.checks.Skill != nil {
				for i, s := range d.Skills {
					if err := r.checks.Skill(s.Name, s.Hash); err != nil {
						found.add(fmt.Sprintf("skills[%d]", i), 0, CodeUnresolved, "%v", err)
					}
				}
			}
			if err := found.asError(d.Location()); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return checkCycles(byName)
}

func unwrapNotFound(err error) string {
	return strings.TrimPrefix(err.Error(), ErrNotFound.Error()+": ")
}

// checkCycles follows each definition's resolved sub-agents and rejects a
// path that returns to a definition already on it.
func checkCycles(byName map[string][]*Definition) error {
	const (
		visiting = 1
		done     = 2
	)
	state := map[*Definition]int{}
	var path []*Definition
	var visit func(d *Definition) error
	visit = func(d *Definition) error {
		switch state[d] {
		case done:
			return nil
		case visiting:
			i := slices.Index(path, d)
			ids := make([]string, 0, len(path)-i+1)
			for _, p := range path[i:] {
				ids = append(ids, p.ID())
			}
			ids = append(ids, d.ID())
			return &ValidationError{Source: d.Location(), Issues: []Issue{{Path: "subagents", Code: CodeCycle,
				Message: "sub-agent cycle: " + strings.Join(ids, " -> ")}}}
		}
		state[d] = visiting
		path = append(path, d)
		for _, s := range d.Subagents {
			ref, err := ParseRef(s.Ref)
			if err != nil {
				continue
			}
			child, err := pick(byName, ref)
			if err != nil {
				continue
			}
			if err := visit(child); err != nil {
				return err
			}
		}
		path = path[:len(path)-1]
		state[d] = done
		return nil
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		for _, d := range byName[n] {
			if err := visit(d); err != nil {
				return err
			}
		}
	}
	return nil
}

// Resolve returns the highest version that satisfies ref (name or
// name@range), with its sub-agents resolved, and pins it.
func (r *Registry) Resolve(ref string) (*Resolved, error) {
	parsed, err := ParseRef(ref)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byName == nil {
		return nil, errors.New("agent definitions: registry not loaded")
	}
	d, err := pick(r.byName, parsed)
	if err != nil {
		return nil, err
	}
	return r.resolveLocked(d)
}

func (r *Registry) resolveLocked(d *Definition) (*Resolved, error) {
	if res, ok := r.resolved[d.ID()]; ok {
		return res, nil
	}
	out := &Resolved{Definition: d}
	h := sha256.New()
	h.Write([]byte(d.Digest))
	for _, s := range d.Subagents {
		ref, err := ParseRef(s.Ref)
		if err != nil {
			return nil, err
		}
		child, err := pick(r.byName, ref)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", d.ID(), err)
		}
		cr, err := r.resolveLocked(child)
		if err != nil {
			return nil, err
		}
		out.Subagents = append(out.Subagents, ResolvedSubagent{SubagentRef: s, Agent: cr})
		fmt.Fprintf(h, "\n%s=%s", s.Ref, cr.Digest)
	}
	out.Digest = "sha256:" + hex.EncodeToString(h.Sum(nil))
	r.resolved[d.ID()] = out
	r.pinned[out.Digest] = out
	return out, nil
}

// Pinned returns a resolution this registry handed out earlier, by its
// digest, even after a reload replaced or removed the definition.
func (r *Registry) Pinned(digest string) (*Resolved, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	res, ok := r.pinned[digest]
	return res, ok
}

// List returns every loaded definition, by name and then highest version
// first.
func (r *Registry) List() []*Definition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.byName))
	for n := range r.byName {
		names = append(names, n)
	}
	slices.Sort(names)
	var out []*Definition
	for _, n := range names {
		out = append(out, r.byName[n]...)
	}
	return out
}

// Revision counts the loads that changed the registry's contents.
func (r *Registry) Revision() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.revision
}

// WatchOptions configures Watch.
type WatchOptions struct {
	// Interval polls the source when it is not a Watcher, or as a backstop
	// when it is. Zero relies on the source's own notifications alone.
	Interval time.Duration
	// OnReload is called after each reload attempt. A failed reload keeps
	// the previous contents.
	OnReload func(changed bool, err error)
}

// Watch reloads the registry when the source reports a change, and every
// Interval. It returns at once; stop ends the watch and waits for a reload
// in progress.
func (r *Registry) Watch(ctx context.Context, opts WatchOptions) (stop func(), err error) {
	ctx, cancel := context.WithCancel(ctx)
	trigger := make(chan struct{}, 1)
	notify := func() {
		select {
		case trigger <- struct{}{}:
		default:
		}
	}
	stopSource, err := watch(ctx, r.src, notify)
	if err != nil {
		cancel()
		return nil, err
	}
	var tick <-chan time.Time
	var ticker *time.Ticker
	if opts.Interval > 0 {
		ticker = time.NewTicker(opts.Interval)
		tick = ticker.C
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if ticker != nil {
			defer ticker.Stop()
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-trigger:
			case <-tick:
			}
			changed, err := r.Load(ctx)
			if ctx.Err() != nil {
				return
			}
			if opts.OnReload != nil {
				opts.OnReload(changed, err)
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			stopSource()
			cancel()
			wg.Wait()
		})
	}, nil
}
