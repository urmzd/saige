// Package credential is the seam through which a connector, or a tool, gets
// the credential it acts with. A caller names the connector and the subject
// the credential belongs to, an app installation or a person, and a Source
// answers with a token. Where tokens come from (a host's own secret store, an
// OAuth refresh, a GitHub App's installation exchange, a hosted credential
// service) is the Source's business; callers never change when it does.
//
// Connectors already take an *http.Client for their API calls. Client builds
// one that asks the Source for a token on every request, so a connector built
// with an empty static token, such as github.NewHTTPAPI("", client), always
// sends the current one.
package credential

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// ErrNoCredential reports that a Source holds no credential for a connector
// and subject. A caller acting for a person who has none acts with nothing:
// it never falls back to another subject's credential.
var ErrNoCredential = errors.New("credential: no credential")

// Kind is whose credential a subject names.
type Kind string

const (
	// KindApp is a credential the deployment holds for a connector: a bot
	// token, an app installation.
	KindApp Kind = "app"
	// KindUser is a person's own credential for a connector.
	KindUser Kind = "user"
)

// Subject is whose credential is asked for. ID is the app installation or
// workspace for KindApp, and the person's id for KindUser.
type Subject struct {
	Kind Kind
	ID   string
}

// App returns the subject of a deployment's credential for installation id.
func App(id string) Subject { return Subject{Kind: KindApp, ID: id} }

// User returns the subject of person id's own credential.
func User(id string) Subject { return Subject{Kind: KindUser, ID: id} }

// Valid reports whether s names a kind and an id.
func (s Subject) Valid() bool { return (s.Kind == KindApp || s.Kind == KindUser) && s.ID != "" }

// String is "<kind>:<id>".
func (s Subject) String() string { return string(s.Kind) + ":" + s.ID }

// Token is a credential's current value. Expiry is zero when the source does
// not know when it ends.
type Token struct {
	Value  string
	Expiry time.Time
}

// String never prints the value.
func (t Token) String() string { return "credential.Token(redacted)" }

// GoString never prints the value.
func (t Token) GoString() string { return t.String() }

// Expired reports whether t has an expiry at or before now.
func (t Token) Expired(now time.Time) bool { return !t.Expiry.IsZero() && !now.Before(t.Expiry) }

// Source answers with the credential connector acts with for subject. It
// returns an error matching ErrNoCredential when it holds none, and never a
// token for a different subject than the one asked for.
type Source interface {
	Token(ctx context.Context, connector string, subject Subject) (Token, error)
}

// SourceFunc adapts a function to a Source.
type SourceFunc func(ctx context.Context, connector string, subject Subject) (Token, error)

// Token calls f.
func (f SourceFunc) Token(ctx context.Context, connector string, subject Subject) (Token, error) {
	return f(ctx, connector, subject)
}

// Static is a Source over fixed tokens, keyed by connector and subject: for
// tests, examples and local runs. It is safe for concurrent use.
type Static struct {
	mu     sync.RWMutex
	tokens map[string]Token
}

// NewStatic returns an empty Static.
func NewStatic() *Static { return &Static{tokens: map[string]Token{}} }

// Set stores value as connector's credential for subject.
func (s *Static) Set(connector string, subject Subject, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[key(connector, subject)] = Token{Value: value}
}

// Token returns the stored token, or ErrNoCredential.
func (s *Static) Token(_ context.Context, connector string, subject Subject) (Token, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tokens[key(connector, subject)]
	if !ok {
		return Token{}, fmt.Errorf("%w: %s for %s", ErrNoCredential, connector, subject)
	}
	return t, nil
}

func key(connector string, subject Subject) string { return connector + "\x00" + subject.String() }

// Client returns an *http.Client whose requests carry
// "Authorization: Bearer <token>", the token asked of src for connector and
// subject on every request. A request it cannot get a token for fails before
// it is sent. base supplies the transport and timeout; nil uses
// http.DefaultClient's.
func Client(src Source, connector string, subject Subject, base *http.Client) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	next := base.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	c := *base
	c.Transport = &transport{src: src, connector: connector, subject: subject, next: next}
	return &c
}

type transport struct {
	src       Source
	connector string
	subject   Subject
	next      http.RoundTripper
}

func (t *transport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !t.subject.Valid() {
		return nil, fmt.Errorf("%w: %s: subject %q is not an app or a user", ErrNoCredential, t.connector, t.subject)
	}
	tok, err := t.src.Token(r.Context(), t.connector, t.subject)
	if err != nil {
		return nil, fmt.Errorf("credential: %s for %s: %w", t.connector, t.subject, err)
	}
	if tok.Value == "" {
		return nil, fmt.Errorf("%w: %s for %s: empty token", ErrNoCredential, t.connector, t.subject)
	}
	out := r.Clone(r.Context())
	out.Header.Set("Authorization", "Bearer "+tok.Value)
	return t.next.RoundTrip(out)
}
