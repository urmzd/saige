// Package inbound is the front door every webhook trigger shares: authenticate
// the request, then hand the raw bytes to whoever knows how to read them.
//
// # The order is the security property
//
// A webhook trigger has no session and no handshake. The only evidence that a
// POST came from Slack rather than from anyone who learned the URL is a MAC over
// the exact bytes of the body, so the bytes are verified before anything looks
// at them. Gate.Body enforces that ordering structurally: it returns bytes only
// after a Verifier accepted them, and a handler that wants the payload has no
// other way to get it.
//
// An absent Verifier is a configuration error, not a permissive setting: a gate
// with no verifier refuses every request. The alternative — treating "no secret
// configured" as "accept everything" — turns a missing environment variable into
// an open door that starts agent runs, and agent runs use tools.
//
// # The Ingest/ServeHTTP seam
//
// pkg/connector's Trigger face is a blocking Ingest that is handed a publish
// function, while an HTTP webhook is a handler that is called by a server the
// trigger does not own. Pipe joins the two: Ingest parks on Pipe.Serve, holding
// the publish function, and each request hands its event to Pipe.Send. Before Ingest is called, or after
// it returns, Send reports ErrNotIngesting and the handler answers 503 — which
// for all three providers means "retry", and their retry carries the same event
// identifier, so the event is not lost and does not duplicate.
//
// This package is stdlib plus pkg/event. It deliberately does not import
// pkg/signature: it declares the one-method interface that package's verifiers
// satisfy, so a connector can be given any authenticator, and a change to the
// signature package cannot break the mesh.
package inbound

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/urmzd/saige/flow/event"
)

// DefaultMaxBody bounds a request body. It is well above the largest realistic
// payload (a GitHub pull_request event on a large repository) and well below
// anything that would let an unauthenticated caller cost us memory, which
// matters because the body must be buffered whole before it can be verified.
const DefaultMaxBody int64 = 4 << 20

// Errors this package returns and maps to statuses.
var (
	// ErrUnauthorized reports a request that failed authentication.
	ErrUnauthorized = errors.New("inbound: unauthorized")
	// ErrNoSecret reports a gate with nothing to authenticate against. It is
	// our misconfiguration, not the sender's, and maps to 500.
	ErrNoSecret = errors.New("inbound: no secret configured")
	// ErrNotIngesting reports a request that arrived while no Ingest was
	// running. It maps to 503 so the provider retries.
	ErrNotIngesting = errors.New("inbound: trigger is not ingesting")
)

// Verifier authenticates a raw request body against its headers.
//
// It is the interface the verifiers in pkg/signature satisfy. Declaring it here
// rather than importing them is what lets a deployment supply its own
// authenticator (a proxy-injected header, a test double) and keeps the
// dependency pointing from the implementation to the interface.
type Verifier interface {
	Verify(header http.Header, body []byte) error
}

// Secret authenticates a shared secret carried in a header.
//
// It exists for Jira, which offers no request signing on its webhooks: the
// secret is whatever the URL's configurer put in a header. That is strictly
// weaker than a MAC — it does not bind the secret to the body, so anyone who
// captures one request can replay a different one — and the honest mitigation is
// the same as everywhere else here: event id idempotency downstream, plus TLS.
type Secret struct {
	// Header is the header carrying the secret.
	Header string
	// Value is the expected secret.
	Value string
}

// Verify compares the header against the expected secret in constant time. The
// comparison is constant time for the same reason a MAC comparison is: the
// endpoint is public and can be probed as often as an attacker likes.
func (s Secret) Verify(header http.Header, _ []byte) error {
	if s.Value == "" || s.Header == "" {
		return fmt.Errorf("%w: shared-secret gate has no header or value", ErrNoSecret)
	}
	got := header.Values(s.Header)
	if len(got) != 1 {
		return fmt.Errorf("%w: %s appears %d times", ErrUnauthorized, s.Header, len(got))
	}
	if subtle.ConstantTimeCompare([]byte(got[0]), []byte(s.Value)) != 1 {
		return fmt.Errorf("%w: %s does not match the configured secret", ErrUnauthorized, s.Header)
	}
	return nil
}

// Gate reads and authenticates request bodies.
type Gate struct {
	// Verifier authenticates the body. A nil Verifier refuses every request.
	Verifier Verifier
	// MaxBody bounds the body in bytes. Zero means DefaultMaxBody.
	MaxBody int64
	// StatusFor maps a verification error to a status code. Nil means every
	// failure answers 401, except a gate with no verifier, which answers 500.
	//
	// The default deliberately does not distinguish a malformed signature from a
	// wrong one: the two are indistinguishable to the sender by design, and a
	// caller that wants the finer mapping (400 for malformed, 401 for mismatch)
	// supplies one, since only the caller knows which sentinel errors its
	// verifier returns.
	StatusFor func(error) int
}

// Body reads, bounds and authenticates the request body.
//
// It answers the request itself on every failure and returns ok=false; a handler
// that gets ok=false must return immediately without writing anything more.
func (g Gate) Body(w http.ResponseWriter, r *http.Request) (body []byte, ok bool) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return nil, false
	}
	if g.Verifier == nil {
		// Fail closed, and say so in the log rather than to the caller.
		http.Error(w, "webhook is not configured", http.StatusInternalServerError)
		return nil, false
	}

	max := g.MaxBody
	if max <= 0 {
		max = DefaultMaxBody
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return nil, false
	}
	if err := g.Verifier.Verify(r.Header, body); err != nil {
		http.Error(w, "unauthorized", g.status(err))
		return nil, false
	}
	return body, true
}

func (g Gate) status(err error) int {
	if g.StatusFor != nil {
		if code := g.StatusFor(err); code != 0 {
			return code
		}
	}
	if errors.Is(err, ErrNoSecret) {
		return http.StatusInternalServerError
	}
	return http.StatusUnauthorized
}

// Pipe carries events from a webhook handler to the Ingest that is running.
//
// The zero Pipe is ready to use. It is safe for concurrent use.
type Pipe struct {
	mu      sync.Mutex
	publish func(context.Context, event.Envelope) error
	done    chan struct{}
}

// Serve claims publish and blocks until ctx is cancelled. It is the whole
// body of a webhook trigger's Ingest.
//
// A second concurrent Serve is refused rather than allowed to take over,
// because two Ingests on one trigger means two configurations disagreeing about
// where events go, and silently honouring the newer one would strand the older.
func (p *Pipe) Serve(ctx context.Context, publish func(context.Context, event.Envelope) error) error {
	if publish == nil {
		return fmt.Errorf("inbound: Ingest requires a publish function")
	}
	p.mu.Lock()
	if p.publish != nil {
		p.mu.Unlock()
		return fmt.Errorf("inbound: already ingesting")
	}
	done := make(chan struct{})
	p.publish, p.done = publish, done
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		p.publish, p.done = nil, nil
		p.mu.Unlock()
		close(done)
	}()

	<-ctx.Done()
	return ctx.Err()
}

// Send publishes e through the running Ingest, under the request's context.
// It reports ErrNotIngesting when no Ingest is running. A publish that fails
// is answered 503 by the handler, and the provider's retry carries the same
// event id.
func (p *Pipe) Send(ctx context.Context, e event.Envelope) error {
	p.mu.Lock()
	publish, done := p.publish, p.done
	p.mu.Unlock()

	if publish == nil {
		return ErrNotIngesting
	}
	select {
	case <-done:
		return ErrNotIngesting
	default:
	}
	if err := publish(ctx, e); err != nil {
		return fmt.Errorf("inbound: publish %s: %w", e.ID, err)
	}
	return nil
}

// Ingesting reports whether an Ingest is currently running. A handler uses it to
// answer a provider's readiness probe honestly.
func (p *Pipe) Ingesting() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.publish != nil
}

// SendStatus is the status a handler should answer when Send fails: 503 while
// the trigger is not ingesting (retry us), 499-equivalent otherwise. There is no
// standard code for "the caller went away", so a cancelled request answers 503
// as well; the provider retries either way and the event id makes that safe.
func SendStatus(error) int { return http.StatusServiceUnavailable }
