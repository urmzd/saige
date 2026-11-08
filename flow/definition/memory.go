package definition

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// Option configures a Memory or Postgres source.
type Option func(*settings)

type settings struct {
	clock  func() time.Time
	checks Checks
}

func newSettings(opts []Option) settings {
	s := settings{clock: time.Now}
	for _, opt := range opts {
		opt(&s)
	}
	return s
}

// WithClock sets the time source CreatedAt is read from.
func WithClock(now func() time.Time) Option {
	return func(s *settings) {
		if now != nil {
			s.clock = now
		}
	}
}

// WithChecks sets the deployment facts Put validates against: the provider
// table and, optionally, the credential check.
func WithChecks(c Checks) Option { return func(s *settings) { s.checks = c } }

// WithProviders sets the provider table Put validates against.
func WithProviders(providers ...string) Option {
	return func(s *settings) { s.checks.Providers = slices.Clone(providers) }
}

// prepare is Prepare with this store's checks and clock.
func (s settings) prepare(rec Record) (Record, error) {
	return Prepare(rec, s.checks, s.clock())
}

// Prepare validates rec as every Source stores it and fills what the store
// owns: the name and version from the content, the digest, the normalized
// policy and its hash, and CreatedAt, set to at. A backend outside this
// package calls it so its rules cannot drift from these.
func Prepare(rec Record, checks Checks, at time.Time) (Record, error) {
	rec.Tenant = strings.TrimSpace(rec.Tenant)
	rec.CreatedBy = strings.TrimSpace(rec.CreatedBy)
	switch {
	case rec.Tenant == "":
		return Record{}, fmt.Errorf("%w: no tenant; every definition is scoped to one", ErrInvalid)
	case rec.CreatedBy == "":
		return Record{}, fmt.Errorf("%w: a definition must say who wrote it", ErrInvalid)
	}
	a, err := Parse(rec.Content, rec.Policy, checks)
	if err != nil {
		return Record{}, err
	}
	d := a.Definition
	if rec.Name != "" && rec.Name != d.Name {
		return Record{}, fmt.Errorf("%w: record names %q but the content defines %q", ErrInvalid, rec.Name, d.Name)
	}
	if rec.Version != "" && rec.Version != d.Version {
		return Record{}, fmt.Errorf("%w: record is version %s but the content is %s", ErrInvalid, rec.Version, d.Version)
	}
	rec.Name, rec.Version, rec.Digest = d.Name, d.Version, d.Digest
	rec.Policy = a.Policy
	rec.PolicyHash = a.Policy.Hash()
	// Postgres keeps microseconds, so a stamp is cut to them here and a
	// record reads back exactly as it was written on every backend.
	rec.CreatedAt = at.UTC().Truncate(time.Microsecond)
	return rec, nil
}

// Same reports whether two records hold the same definition: the same
// content digest and the same policy hash.
func Same(a, b Record) bool { return a.same(b) }

// SortVersions orders records lowest version first, as Versions returns them.
func SortVersions(list []Record) { sortVersions(list) }

// Memory is a Source held in this process. It keeps every rule Postgres
// keeps and is held to them by the same suite (definitiontest).
type Memory struct {
	settings
	mu      sync.RWMutex
	tenants map[string]map[string]*entry
}

type entry struct {
	versions []Record // lowest first
	retired  bool
}

// NewMemory returns an empty source.
func NewMemory(opts ...Option) *Memory {
	return &Memory{settings: newSettings(opts), tenants: map[string]map[string]*entry{}}
}

var _ Source = (*Memory)(nil)

// Put implements Source.
func (m *Memory) Put(_ context.Context, rec Record) (Record, error) {
	rec, err := m.prepare(rec)
	if err != nil {
		return Record{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	names := m.tenants[rec.Tenant]
	if names == nil {
		names = map[string]*entry{}
		m.tenants[rec.Tenant] = names
	}
	e := names[rec.Name]
	if e == nil {
		e = &entry{}
		names[rec.Name] = e
	}
	for _, have := range e.versions {
		if have.Version != rec.Version {
			continue
		}
		if have.same(rec) {
			return clone(have), nil
		}
		return Record{}, fmt.Errorf("%w: %s@%s holds other content; a version is never rewritten", ErrExists, rec.Name, rec.Version)
	}
	if e.retired {
		return Record{}, fmt.Errorf("%w: %s", ErrRetired, rec.Name)
	}
	e.versions = append(e.versions, clone(rec))
	sortVersions(e.versions)
	return clone(rec), nil
}

func (m *Memory) entry(tenant, name string) *entry {
	return m.tenants[tenant][name]
}

// Get implements Source.
func (m *Memory) Get(_ context.Context, tenant, name, version string) (Record, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if e := m.entry(tenant, name); e != nil {
		for _, r := range e.versions {
			if r.Version == version {
				return clone(r), nil
			}
		}
	}
	return Record{}, fmt.Errorf("%w: %s@%s", ErrNotFound, name, version)
}

// Versions implements Source.
func (m *Memory) Versions(_ context.Context, tenant, name string) ([]Record, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e := m.entry(tenant, name)
	if e == nil || len(e.versions) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	out := make([]Record, len(e.versions))
	for i, r := range e.versions {
		out[i] = clone(r)
	}
	return out, nil
}

// Latest implements Source.
func (m *Memory) Latest(_ context.Context, tenant, name string) (Record, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e := m.entry(tenant, name)
	switch {
	case e == nil || len(e.versions) == 0:
		return Record{}, fmt.Errorf("%w: %s", ErrNotFound, name)
	case e.retired:
		return Record{}, fmt.Errorf("%w: %s", ErrRetired, name)
	}
	return clone(e.versions[len(e.versions)-1]), nil
}

// List implements Source.
func (m *Memory) List(_ context.Context, tenant string, page Page) ([]Record, string, error) {
	after, err := decodeToken(page.Token, tenant)
	if err != nil {
		return nil, "", err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.tenants[tenant]))
	for name, e := range m.tenants[tenant] {
		if !e.retired && len(e.versions) > 0 && name > after {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	size := page.size()
	next := ""
	if len(names) > size {
		names = names[:size]
		next = encodeToken(tenant, names[size-1])
	}
	out := make([]Record, 0, len(names))
	for _, name := range names {
		e := m.tenants[tenant][name]
		out = append(out, clone(e.versions[len(e.versions)-1]))
	}
	return out, next, nil
}

// Retire implements Source.
func (m *Memory) Retire(_ context.Context, tenant, name, by string) error {
	if strings.TrimSpace(by) == "" {
		return fmt.Errorf("%w: a retirement must say who made it", ErrInvalid)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entry(tenant, name)
	if e == nil || len(e.versions) == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	e.retired = true
	return nil
}

func clone(r Record) Record {
	r.Policy = r.Policy.Normalize()
	return r
}
