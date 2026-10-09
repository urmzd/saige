// Package memory gives agents durable, scoped memory across conversations.
//
// The host owns scope: Policy.Scope maps the calling agent to a Scope, and the
// model never supplies a tenant. Writes are agent-initiated through tools that
// ask for human approval by default, and every write passes the policy's
// content check, which fails closed on sensitive input unless a redactor is
// configured. Recall returns records as external input, wrapped so the text
// cannot pose as an instruction, and is never added as system content.
//
// Stores: MemStore for tests and fixtures, FileStore for markdown files on
// disk (with the six file commands under /memories), and KGStore over a
// knowledge graph.
package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/urmzd/saige/agent/privacy"
)

var (
	// ErrNoScope is returned when no scope can be resolved for a call.
	ErrNoScope = errors.New("memory: no scope")
	// ErrReadOnly is returned by writes to a read-only scope.
	ErrReadOnly = errors.New("memory: scope is read-only")
	// ErrNotFound is returned when a record or path does not exist.
	ErrNotFound = errors.New("memory: not found")
	// ErrSensitive is returned when content holds sensitive values and the
	// policy has no redactor.
	ErrSensitive = errors.New("memory: content holds sensitive values")
	// ErrKindNotAllowed is returned when the policy does not admit a kind.
	ErrKindNotAllowed = errors.New("memory: kind not allowed")
	// ErrUnsupported is returned when a store cannot perform an operation.
	ErrUnsupported = errors.New("memory: operation not supported")
)

// Kind classifies a memory.
type Kind string

const (
	// KindSemantic is a fact about the user, project, or world.
	KindSemantic Kind = "semantic"
	// KindEpisodic records what happened, with its provenance.
	KindEpisodic Kind = "episodic"
	// KindProcedural records how to do something.
	KindProcedural Kind = "procedural"
)

// Valid reports whether k is a known kind.
func (k Kind) Valid() bool {
	return k == KindSemantic || k == KindEpisodic || k == KindProcedural
}

// Scope partitions memory. The host sets it; tools never take it from the
// model.
type Scope struct {
	Tenant    string // required isolation boundary
	Subject   string // user or project within the tenant
	Namespace string // optional sub-partition, "/"-separated
	// ReadOnly rejects writes. Give sub-agents a read-only scope so they can
	// recall but not change what their caller remembers.
	ReadOnly bool
}

// Validate reports whether the scope can address records.
func (s Scope) Validate() error {
	if strings.TrimSpace(s.Tenant) == "" {
		return fmt.Errorf("%w: tenant is required", ErrNoScope)
	}
	for _, part := range strings.Split(s.Namespace, "/") {
		if s.Namespace != "" && (part == "" || part == "." || part == "..") {
			return fmt.Errorf("%w: invalid namespace %q", ErrNoScope, s.Namespace)
		}
	}
	return nil
}

// Key is a stable string form of the scope, without ReadOnly.
func (s Scope) Key() string {
	return s.Tenant + "\x00" + s.Subject + "\x00" + s.Namespace
}

// Narrow returns a scope for a sub-namespace. A read-only scope stays
// read-only.
func (s Scope) Narrow(namespace string) Scope {
	if namespace == "" {
		return s
	}
	if s.Namespace == "" {
		s.Namespace = namespace
	} else {
		s.Namespace += "/" + namespace
	}
	return s
}

// AsReadOnly returns s with writes disabled.
func (s Scope) AsReadOnly() Scope {
	s.ReadOnly = true
	return s
}

// contains reports whether a record stored under o is visible from s: the
// same tenant and subject, and o's namespace equal to or under s's.
func (s Scope) contains(o Scope) bool {
	if s.Tenant != o.Tenant || s.Subject != o.Subject {
		return false
	}
	return s.Namespace == "" || o.Namespace == s.Namespace || strings.HasPrefix(o.Namespace, s.Namespace+"/")
}

// Provenance records where a memory came from.
type Provenance struct {
	Agent        string `json:"agent,omitempty"`
	ToolCallID   string `json:"tool_call_id,omitempty"`
	Conversation string `json:"conversation,omitempty"`
	Source       string `json:"source,omitempty"` // free-form, e.g. "post-run extraction"
}

// Record is one memory.
type Record struct {
	ID        string     `json:"id"`
	Scope     Scope      `json:"-"`
	Kind      Kind       `json:"kind"`
	Content   string     `json:"-"`
	Tags      []string   `json:"tags,omitempty"`
	Source    Provenance `json:"source,omitempty"`
	CreatedAt time.Time  `json:"created"`
	ExpiresAt time.Time  `json:"expires,omitempty"`
	// IdempotencyKey makes Remember safe to repeat: the same scope and key
	// always yield the same ID and write once. Derive it from the step that
	// writes, such as the tool call ID, so a replayed step does not duplicate
	// the memory.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// Expired reports whether r has expired at now.
func (r Record) Expired(now time.Time) bool {
	return !r.ExpiresAt.IsZero() && !now.Before(r.ExpiresAt)
}

// Store keeps memories.
type Store interface {
	// Remember stores r and returns its ID. With an IdempotencyKey, a repeat
	// returns the first ID and stores nothing new.
	Remember(ctx context.Context, r Record) (string, error)
	// Recall returns the records in s (and its sub-namespaces) that best
	// match query, most relevant first, within budget tokens. An empty query
	// returns the most recent records.
	Recall(ctx context.Context, s Scope, query string, budget int) ([]Record, error)
	// Forget deletes a record.
	Forget(ctx context.Context, s Scope, id string) error
}

// DefaultRecallBudget is the token budget Recall uses when given 0.
const DefaultRecallBudget = 1000

// EstimateTokens approximates the tokens in text at four bytes per token.
func EstimateTokens(text string) int { return (len(text) + 3) / 4 }

// recordID derives a stable ID from the scope and the idempotency key, or
// from the content when there is no key.
func recordID(r Record) string {
	basis := r.IdempotencyKey
	if basis == "" {
		basis = "content\x00" + string(r.Kind) + "\x00" + r.Content
	}
	sum := sha256.Sum256([]byte(r.Scope.Key() + "\x00" + basis))
	return "mem-" + hex.EncodeToString(sum[:10])
}

// rank orders candidates for query and keeps those that fit budget.
func rank(records []Record, query string, budget int, now time.Time) []Record {
	if budget <= 0 {
		budget = DefaultRecallBudget
	}
	terms := strings.Fields(strings.ToLower(query))
	type scored struct {
		Record
		score int
	}
	var cands []scored
	for _, r := range records {
		if r.Expired(now) {
			continue
		}
		score := 0
		if len(terms) > 0 {
			hay := strings.ToLower(r.Content + " " + strings.Join(r.Tags, " "))
			for _, t := range terms {
				if strings.Contains(hay, t) {
					score++
				}
			}
			if score == 0 {
				continue
			}
		}
		cands = append(cands, scored{r, score})
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.score != b.score {
			return a.score > b.score
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.After(b.CreatedAt)
		}
		return a.ID < b.ID
	})
	var out []Record
	used := 0
	for _, c := range cands {
		cost := EstimateTokens(c.Content)
		if used+cost > budget {
			if len(out) == 0 {
				continue // a record larger than the whole budget is skipped
			}
			break
		}
		used += cost
		out = append(out, c.Record)
	}
	return out
}

// RecallMode chooses how memories reach the model.
type RecallMode int

const (
	// RecallByTool exposes the recall tool; the model asks when it needs to.
	RecallByTool RecallMode = iota
	// RecallByInjection adds matching memories as a user message at the start of
	// a run, through InjectMessage.
	RecallByInjection
	// RecallDisabled disables recall.
	RecallDisabled
)

// AllowList admits memory kinds. Empty admits every kind.
type AllowList []Kind

// Allows reports whether k is admitted.
func (a AllowList) Allows(k Kind) bool {
	if len(a) == 0 {
		return true
	}
	for _, x := range a {
		if x == k {
			return true
		}
	}
	return false
}

// Policy governs memory tools and host-initiated writes.
type Policy struct {
	// Write admits the kinds the model may write. Empty admits all.
	Write AllowList
	// AutoApprove removes the approval marker from write tools. The default
	// asks a human before every write.
	AutoApprove bool
	// Recall chooses how memories reach the model.
	Recall RecallMode
	// Scope maps the calling agent to its scope. Required: without it every
	// memory tool fails with ErrNoScope.
	Scope func(ctx context.Context, owner string) (Scope, error)
	// Retention sets ExpiresAt on new records. 0 keeps them until forgotten.
	Retention time.Duration
	// Redact rewrites content before it is stored, for example with
	// privacy.Redact. Without it, content in which Detector finds anything
	// is rejected with ErrSensitive.
	Redact func(ctx context.Context, text string) (string, error)
	// Detector finds sensitive content when Redact is nil. nil uses
	// privacy.DefaultDetector.
	Detector privacy.Detector
	// Now returns the current time. nil uses time.Now.
	Now func() time.Time
}

func (p Policy) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// ResolveScope returns the scope for owner.
func (p Policy) ResolveScope(ctx context.Context, owner string) (Scope, error) {
	if p.Scope == nil {
		return Scope{}, ErrNoScope
	}
	s, err := p.Scope(ctx, owner)
	if err != nil {
		return Scope{}, err
	}
	if err := s.Validate(); err != nil {
		return Scope{}, err
	}
	return s, nil
}

// CheckContent applies the content rule to text bound for scope s and
// returns what may be stored.
func (p Policy) CheckContent(ctx context.Context, s Scope, text string) (string, error) {
	if s.ReadOnly {
		return "", ErrReadOnly
	}
	if p.Redact != nil {
		return p.Redact(ctx, text)
	}
	d := p.Detector
	if d == nil {
		d = privacy.DefaultDetector()
	}
	spans, err := d.Detect(ctx, text)
	if err != nil {
		return "", fmt.Errorf("memory: check content: %w", err)
	}
	if len(spans) > 0 {
		found := make([]string, 0, len(spans))
		for _, sp := range spans {
			found = append(found, sp.Label)
		}
		return "", fmt.Errorf("%w (%s); configure Policy.Redact to store redacted text", ErrSensitive, strings.Join(dedupe(found), ", "))
	}
	return text, nil
}

// CheckName applies the content rule to a name that becomes part of what is
// stored, such as a memory file path. A name cannot be redacted and still
// mean the same thing, so a name the redactor would change is rejected with
// ErrSensitive.
func (p Policy) CheckName(ctx context.Context, s Scope, name string) error {
	clean, err := p.CheckContent(ctx, s, name)
	if err != nil {
		return err
	}
	if clean != name {
		return fmt.Errorf("%w: in name %q; choose a name without personal data", ErrSensitive, clean)
	}
	return nil
}

// Remember checks r against the policy and stores it: the scope must be
// writable, the kind admitted, and the content and tags clean or redacted. Retention
// sets ExpiresAt when r has none.
func (p Policy) Remember(ctx context.Context, store Store, r Record) (string, error) {
	if err := r.Scope.Validate(); err != nil {
		return "", err
	}
	if r.Kind == "" {
		r.Kind = KindSemantic
	}
	if !r.Kind.Valid() {
		return "", fmt.Errorf("memory: unknown kind %q", r.Kind)
	}
	if !p.Write.Allows(r.Kind) {
		return "", fmt.Errorf("%w: %s", ErrKindNotAllowed, r.Kind)
	}
	if strings.TrimSpace(r.Content) == "" {
		return "", fmt.Errorf("memory: empty content")
	}
	content, err := p.CheckContent(ctx, r.Scope, r.Content)
	if err != nil {
		return "", err
	}
	r.Content = content
	// Tags are stored and shown beside the content, so they follow the
	// same rule.
	if len(r.Tags) > 0 {
		tags := make([]string, len(r.Tags))
		for i, tag := range r.Tags {
			if tags[i], err = p.CheckContent(ctx, r.Scope, tag); err != nil {
				return "", err
			}
		}
		r.Tags = tags
	}
	now := p.now().UTC()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = now
	}
	if r.ExpiresAt.IsZero() && p.Retention > 0 {
		r.ExpiresAt = r.CreatedAt.Add(p.Retention)
	}
	return store.Remember(ctx, r)
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
