// Package definition is where flow's agents come from: saige definitions
// (.agent.md files) plus a flow Policy sidecar, kept per tenant in an
// insert-only Source, and pinned by a run so a replay never runs a newer
// prompt.
//
// saige owns what an agent is (saige/agent/definition): its name, its
// semantic version, its model, its tools, its sub-agents and its approval
// rules. The Policy holds what saige does not: the channels it listens on,
// the workspace areas and MCP write grants it holds, who may approve or
// answer its questions, and when it falls back to another model. A zero
// Policy grants nothing.
//
// # Insert only
//
// A version, once written, never changes. Put of an identical record is a
// no-op and Put of different content under an existing version is
// ErrExists. Retire stops new runs of a name and keeps every version
// readable, so a run pinned to one can always finish.
//
// # Pins
//
// Resolve picks the highest version of a name and every sub-agent it
// reaches, and returns a Pin: the exact versions, saige's resolved digest,
// the policy hash and one hash over all of them. Verify reloads exactly
// those versions and recomputes both hashes. Any mismatch, missing row or
// empty digest fails terminally (durable.NonRetryable): the run never
// executes a definition it did not pin.
package definition

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
)

// Errors callers match with errors.Is.
var (
	// ErrNotFound reports a tenant, name or version with no record.
	ErrNotFound = errors.New("definition: not found")
	// ErrExists reports a Put of different content under a version that
	// already exists.
	ErrExists = errors.New("definition: already exists")
	// ErrConflict reports a write drafted against a version that is no
	// longer the latest.
	ErrConflict = errors.New("definition: conflict")
	// ErrInvalid reports a definition or policy flow will not store or run.
	ErrInvalid = errors.New("definition: invalid")
	// ErrRetired reports a name that was retired: it takes no new version
	// and starts no new run.
	ErrRetired = errors.New("definition: retired")
	// ErrPin reports a pin whose definition no longer matches it.
	ErrPin = errors.New("definition: pin does not hold")
)

// Record is one stored version of one agent in one tenant.
type Record struct {
	Tenant string `json:"tenant"`
	// Name and Version are the definition's own, read from Content.
	Name    string `json:"name"`
	Version string `json:"version"`
	// Content is the .agent.md file: YAML frontmatter and the prompt.
	Content string `json:"content"`
	// Digest is saige's digest of Content ("sha256:<hex>"). The store
	// computes it; a caller's value is ignored.
	Digest string `json:"digest"`
	// Policy is the flow sidecar, and PolicyHash its Hash.
	Policy     Policy    `json:"policy"`
	PolicyHash string    `json:"policy_hash"`
	CreatedAt  time.Time `json:"created_at"`
	CreatedBy  string    `json:"created_by"`
}

// same reports whether two records hold the same definition.
func (r Record) same(o Record) bool {
	return r.Digest == o.Digest && r.PolicyHash == o.PolicyHash
}

// Source keeps definitions per tenant. Every method is scoped to the tenant
// it is given, and a record in another tenant is ErrNotFound.
type Source interface {
	// Put inserts a version. A version that exists with the same content
	// and policy is returned as it is; with anything else it is ErrExists.
	// A retired name is ErrRetired.
	Put(ctx context.Context, rec Record) (Record, error)
	// Get returns one exact version, retired or not.
	Get(ctx context.Context, tenant, name, version string) (Record, error)
	// Versions returns every version of a name, lowest first, retired or
	// not.
	Versions(ctx context.Context, tenant, name string) ([]Record, error)
	// Latest returns the highest version of a name, or ErrRetired.
	Latest(ctx context.Context, tenant, name string) (Record, error)
	// List returns the latest version of each live name, by name, a page at
	// a time. The token it returns is bound to tenant.
	List(ctx context.Context, tenant string, page Page) ([]Record, string, error)
	// Retire stops new runs of a name. Get and Versions keep working.
	Retire(ctx context.Context, tenant, name, by string) error
}

// Page sizes.
const (
	DefaultPageSize = 50
	MaxPageSize     = 500
)

// Page is one request for a page of List.
type Page struct {
	Size  int
	Token string
}

// Limit is the page size a List uses: DefaultPageSize for none, clamped to
// MaxPageSize.
func (p Page) Limit() int { return p.size() }

func (p Page) size() int {
	switch {
	case p.Size <= 0:
		return DefaultPageSize
	case p.Size > MaxPageSize:
		return MaxPageSize
	}
	return p.Size
}

// NormalizeName is how a name a person typed is looked up: trimmed and
// lowercased. saige's grammar is lowercase, so this never makes a valid name
// invalid.
func NormalizeName(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// byVersion orders records lowest version first.
func byVersion(a, b Record) int {
	va, ea := parseVersion(a.Version)
	vb, eb := parseVersion(b.Version)
	if ea != nil || eb != nil {
		return strings.Compare(a.Version, b.Version)
	}
	return va.Compare(vb)
}

func sortVersions(list []Record) { slices.SortStableFunc(list, byVersion) }
