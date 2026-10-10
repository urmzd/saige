// Package approvals holds approvals that wait for a person outside the
// client that asked for them. When an MCP client cannot ask its user (it
// has no elicitation), saige-mcp holds the agent's run, records the pending
// call here under a token, and tells the model to have the user decide with
// saige approvals. The decision is written next to the record, and the
// held run reads it when the model resumes.
//
// The store is a directory of JSON files, private to the user: the pending
// record <token>.json and the decision <token>.decision.json.
package approvals

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/urmzd/saige/agent/definition"
	"github.com/urmzd/saige/agent/types"
)

// EnvDir overrides the store directory.
const EnvDir = "SAIGE_APPROVALS_DIR"

// Errors the store returns.
var (
	// ErrNotFound is returned for a token with no pending record.
	ErrNotFound = errors.New("no pending approval with that token")
	// ErrDecided is returned when a token already has a decision.
	ErrDecided = errors.New("the approval was already decided")
	// ErrExpired is returned for a pending record past its expiry.
	ErrExpired = errors.New("the approval expired")
	// ErrToken is returned for a malformed token.
	ErrToken = errors.New("malformed approval token")
)

var tokenRE = regexp.MustCompile(`^apr_[0-9a-f]{32}$`)

// NewToken returns a fresh, unguessable token.
func NewToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "apr_" + hex.EncodeToString(b[:])
}

// CheckToken rejects a token that NewToken could not have made, so a token
// never names a path outside the store.
func CheckToken(token string) error {
	if !tokenRE.MatchString(token) {
		return fmt.Errorf("%w: %q", ErrToken, token)
	}
	return nil
}

// Pending is a call that waits for a decision.
type Pending struct {
	Token string `json:"token"`
	// Agent is the pinned definition, name@version, or the agent tool's
	// name.
	Agent     string         `json:"agent"`
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments,omitempty"`
	Message   string         `json:"message,omitempty"`
	// MaxGrant is the widest grant the agent's definition allows; empty
	// allows every scope.
	MaxGrant  types.GrantScope `json:"max_grant,omitempty"`
	CreatedAt time.Time        `json:"created_at"`
	ExpiresAt time.Time        `json:"expires_at"`
}

// Decision is a person's answer to a pending call.
type Decision struct {
	Approved bool                `json:"approved"`
	Grant    *types.GrantRequest `json:"grant,omitempty"`
	Message  string              `json:"message,omitempty"`
	// Approver names who decided, such as the OS user.
	Approver  string    `json:"approver,omitempty"`
	DecidedAt time.Time `json:"decided_at"`
}

// Entry is a pending record with its decision, if any.
type Entry struct {
	Pending
	Decision *Decision `json:"decision,omitempty"`
}

// Store is a directory of approvals.
type Store struct {
	dir string
	now func() time.Time
}

// DefaultDir is $SAIGE_APPROVALS_DIR, else $XDG_STATE_HOME/saige/approvals,
// else ~/.local/state/saige/approvals.
func DefaultDir(getenv func(string) string) string {
	if d := getenv(EnvDir); d != "" {
		return d
	}
	state := getenv("XDG_STATE_HOME")
	if state == "" {
		home := getenv("HOME")
		if home == "" {
			return ""
		}
		state = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(state, "saige", "approvals")
}

// Open returns the store at dir, creating it private to the user.
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("approvals: no store directory; set " + EnvDir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("approvals: %w", err)
	}
	return &Store{dir: dir, now: time.Now}, nil
}

// Dir returns the store's directory.
func (s *Store) Dir() string { return s.dir }

func (s *Store) pendingPath(token string) string { return filepath.Join(s.dir, token+".json") }
func (s *Store) decisionPath(token string) string {
	return filepath.Join(s.dir, token+".decision.json")
}

// Put records a pending call.
func (s *Store) Put(p Pending) error {
	if err := CheckToken(p.Token); err != nil {
		return err
	}
	return writeJSON(s.pendingPath(p.Token), p)
}

// Get returns the pending record for token and its decision, if any.
func (s *Store) Get(token string) (Entry, error) {
	if err := CheckToken(token); err != nil {
		return Entry{}, err
	}
	var e Entry
	if err := readJSON(s.pendingPath(token), &e.Pending); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Entry{}, fmt.Errorf("%w: %s", ErrNotFound, token)
		}
		return Entry{}, err
	}
	var d Decision
	switch err := readJSON(s.decisionPath(token), &d); {
	case err == nil:
		e.Decision = &d
	case !errors.Is(err, fs.ErrNotExist):
		return Entry{}, err
	}
	return e, nil
}

// List returns every record, oldest first.
func (s *Store) List() ([]Entry, error) {
	names, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("approvals: %w", err)
	}
	var out []Entry
	for _, n := range names {
		token, ok := strings.CutSuffix(n.Name(), ".json")
		if !ok || CheckToken(token) != nil {
			continue
		}
		e, err := s.Get(token)
		if err != nil {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// Decide records a decision for token. The grant must be well formed and
// within the definition's cap; a token decides once.
func (s *Store) Decide(token string, d Decision) error {
	e, err := s.Get(token)
	if err != nil {
		return err
	}
	if e.Decision != nil {
		return fmt.Errorf("%w: %s", ErrDecided, token)
	}
	now := s.now()
	if !e.ExpiresAt.IsZero() && now.After(e.ExpiresAt) {
		return fmt.Errorf("%w: %s", ErrExpired, token)
	}
	if d.Grant != nil {
		if !d.Approved {
			return errors.New("a grant requires an approval")
		}
		if err := d.Grant.Validate(); err != nil {
			return err
		}
		if e.MaxGrant != "" && definition.GrantRank(d.Grant.Scope) > definition.GrantRank(e.MaxGrant) {
			return fmt.Errorf("%w: agent %s allows grants up to %q, not %q", types.ErrInvalidGrant, e.Agent, e.MaxGrant, d.Grant.Scope)
		}
	}
	d.DecidedAt = now
	// The decision is written whole to a temporary file, then linked into
	// place: the link fails if a decision exists, so the first one wins and
	// a reader never sees a partial file.
	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("approvals: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("approvals: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("approvals: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return fmt.Errorf("approvals: %w", err)
	}
	if err := os.Link(tmp.Name(), s.decisionPath(token)); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%w: %s", ErrDecided, token)
		}
		return fmt.Errorf("approvals: %w", err)
	}
	return nil
}

// Remove deletes the record for token and its decision.
func (s *Store) Remove(token string) error {
	if err := CheckToken(token); err != nil {
		return err
	}
	var errs []error
	for _, p := range []string{s.pendingPath(token), s.decisionPath(token)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// writeJSON writes v to path atomically, private to the user.
func writeJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return fmt.Errorf("approvals: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("approvals: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("approvals: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return fmt.Errorf("approvals: %w", err)
	}
	return os.Rename(tmp.Name(), path)
}

func readJSON(path string, v any) error {
	raw, err := os.ReadFile(path) //nolint:gosec // the path is built from a checked token
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}
