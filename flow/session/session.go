// Package session names an agent's continuing conversation and its tree.
// It owns identity and relationships; thread owns the append-only transcript,
// and durable owns execution and terminal state. Stores never authorize callers:
// the embedding host checks access before using a tenant-scoped handle.
package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/urmzd/saige/flow/thread"
)

var (
	ErrInvalid  = errors.New("session: invalid")
	ErrNotFound = errors.New("session: not found")
	ErrDenied   = errors.New("session: access denied")
)

const MaxPage = 200

// Key is stable across messages, runs, retries and changes of surface.
// Parent is either a canonical place URI or the ID of another session.
type Key struct {
	Tenant string `json:"tenant"`
	Parent string `json:"parent"`
	Agent  string `json:"agent"`
	Label  string `json:"label,omitempty"`
}

func (k Key) Normalize() Key {
	k.Agent = strings.ToLower(strings.TrimSpace(k.Agent))
	k.Parent = strings.TrimSpace(k.Parent)
	k.Label = strings.TrimSpace(k.Label)
	return k
}
func (k Key) Validate() error {
	if k.Tenant == "" || k.Parent == "" || k.Agent == "" || len(k.Parent) > 4096 || len(k.Label) > 128 {
		return fmt.Errorf("%w: tenant, parent and agent are required; parent and label must be bounded", ErrInvalid)
	}
	return nil
}
func (k Key) ID() string {
	b, _ := json.Marshal(k.Normalize())
	sum := sha256.Sum256(append([]byte("fiber/session/v1\x00"), b...))
	return "s_" + hex.EncodeToString(sum[:16])
}

// Open continues a key by default. Fresh generates a label before persisting it.
// Channel is the delivery address, independent of the identity's parent.
type Open struct {
	Parent  string `json:"parent"`
	Agent   string `json:"agent"`
	Label   string `json:"label,omitempty"`
	Fresh   bool   `json:"fresh,omitempty"`
	Channel string `json:"channel,omitempty"`
}

type Record struct {
	ID        string    `json:"id"`
	Key       Key       `json:"key"`
	Channel   string    `json:"channel"`
	Depth     int       `json:"depth"`
	CreatedAt time.Time `json:"created_at"`
}

// ThreadKey preserves the existing journal of unlabelled surface sessions.
// Children and labelled sessions get isolated journals in the same store.
func (r Record) ThreadKey() thread.Key {
	surface := r.Key.Parent
	if r.Depth > 0 || r.Key.Label != "" {
		surface = "session://" + url.PathEscape(r.Key.Tenant) + "/" + r.ID
	}
	return thread.Key{Tenant: r.Key.Tenant, Agent: r.Key.Agent, Surface: surface}
}

type Filter struct {
	Parent string
	Agent  string
	Limit  int
}

type Link struct {
	Target    string    `json:"target"`
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"created_at"`
}

// Store records immutable identities and append-only relationships. Every list
// is capped server-side. Run status is deliberately read from the run ledger.
type Store interface {
	Open(context.Context, string, Open) (Record, error)
	Get(context.Context, string, string) (Record, error)
	List(context.Context, string, Filter) ([]Record, error)
	Link(context.Context, string, string, Link) error
	Links(context.Context, string, string, int) ([]Link, error)
	BindRun(context.Context, string, string, string) error
	Runs(context.Context, string, string, int) ([]string, error)
}

func prepare(tenant string, o Open) (Key, error) {
	k := Key{Tenant: tenant, Parent: o.Parent, Agent: o.Agent, Label: o.Label}.Normalize()
	if o.Fresh {
		var b [12]byte
		if _, err := rand.Read(b[:]); err != nil {
			return Key{}, err
		}
		k.Label = hex.EncodeToString(b[:])
	}
	return k, k.Validate()
}
func limit(n int) int {
	if n <= 0 || n > MaxPage {
		return MaxPage
	}
	return n
}
func channel(k Key, c string) string {
	if c != "" {
		return c
	}
	return "session://" + url.PathEscape(k.Tenant) + "/" + k.ID()
}
func validLink(l Link) bool { return l.Target != "" && (l.Kind == "context" || l.Kind == "evidence") }
