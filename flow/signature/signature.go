// Package signature authenticates inbound webhook requests.
//
// This is a security boundary, and for a webhook-style trigger it is the only
// one. There is no session and no handshake: the sole evidence that a POST
// came from Slack or GitHub, rather than from anyone on the internet who
// learned the URL, is a MAC over the raw request body computed with a shared
// secret. A run, the agent's tools, and whatever those tools are allowed to do
// all hang off this check returning nil, so it fails closed on every
// ambiguity. A missing header, a duplicated header, an unparseable digest, an
// unconfigured secret, and a wrong digest all return an error; nothing returns
// nil on doubt.
//
// # The caller's obligations
//
// Verify the bytes you received, before you interpret them.
//
//	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
//	if err != nil {
//		http.Error(w, "bad request", http.StatusBadRequest)
//		return
//	}
//	if err := verifier.Verify(r.Header, body); err != nil {
//		http.Error(w, "unauthorized", statusFor(err))
//		return
//	}
//	var payload eventPayload            // only now, on bytes known to be authentic
//	if err := json.Unmarshal(body, &payload); err != nil { ... }
//
// Two rules follow from the MAC covering exact bytes:
//
//   - Verify BEFORE parsing. Unmarshalling first runs attacker-controlled
//     input through a decoder and, worse, tempts a handler into reading a
//     field ("it's just the event type") before anything has been
//     authenticated. A handler that acts on unverified input has already lost,
//     whatever it does afterwards.
//   - Hash the EXACT bytes received. Not a re-encoded struct, not a
//     whitespace-normalized copy, not a form-decoded map. None of those
//     reproduce the sender's digest, and the tempting fix — loosening the
//     comparison until it passes — deletes the boundary rather than fixing it.
//
// # Replay
//
// Slack signs a timestamp alongside the body and documents a five minute
// replay window, so a captured request stops being useful once it ages out;
// Slack enforces that here (see Slack.Window).
//
// GitHub signs the body alone. A GitHub webhook carries no timestamp in the
// signing base string, so a captured request stays valid for as long as the
// secret does and no amount of code in this package can change that. The
// honest statement of where that defense lives: downstream, in event id
// idempotency. A replayed or redelivered GitHub event carries the same
// delivery identity, which becomes the run's idempotency key, so it joins the
// run that already exists instead of starting a second one. Verification
// answers "did GitHub send these bytes"; idempotency answers "how many times
// does that count".
//
// # Orthogonality
//
// This package knows about bytes, headers, secrets, and a clock. It knows
// nothing about topics, envelopes, connectors, or what a verified request
// should become — connectors own that. It only answers whether the claimed
// sender sent exactly these bytes, and for Slack, whether it did so recently.
//
// Verifiers are immutable once constructed and safe for concurrent use.
//
// This package is a leaf: stdlib only.
package signature

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Errors returned by verifiers. They are distinct because they map to
// different answers: the request was forged, the request is too old, the
// request is not well formed, or this service is misconfigured.
//
// A handler should translate them, and should not leak which one it saw beyond
// the status code:
//
//	ErrSignatureMismatch -> 401 Unauthorized
//	ErrTimestampSkew     -> 401 Unauthorized
//	ErrMalformed         -> 400 Bad Request
//	ErrNoSecret          -> 500 Internal Server Error (our fault, not theirs)
var (
	// ErrSignatureMismatch reports a digest that does not match the body
	// under the configured secret. The request is forged, replayed with a
	// tampered body, or signed with a secret we do not hold.
	ErrSignatureMismatch = errors.New("signature: mismatch")

	// ErrTimestampSkew reports an authentic-looking timestamp outside the
	// replay window. The signature is not even checked in this case: an old
	// request is refused whether or not its digest is good, which is the
	// entire point of a replay window.
	ErrTimestampSkew = errors.New("signature: timestamp outside replay window")

	// ErrMalformed reports a header that cannot be interpreted: missing,
	// empty, duplicated, missing its algorithm prefix, not hex, or not the
	// length of the digest it claims to be.
	ErrMalformed = errors.New("signature: malformed")

	// ErrNoSecret reports a verifier constructed without a secret. An empty
	// secret is not a permissive setting, it is a broken one: HMAC with an
	// empty key is perfectly computable by an attacker, so a verifier in that
	// state would authenticate the internet. It refuses instead.
	ErrNoSecret = errors.New("signature: no secret configured")
)

// Header names carrying the evidence.
const (
	HeaderSlackSignature = "X-Slack-Signature"
	HeaderSlackTimestamp = "X-Slack-Request-Timestamp"

	// HeaderGitHubSignature is the SHA-256 signature. GitHub also sends an
	// X-Hub-Signature computed with SHA-1, which this package deliberately
	// ignores and never falls back to: a fallback would let a caller who can
	// suppress one header downgrade the whole check.
	HeaderGitHubSignature = "X-Hub-Signature-256"
)

// Algorithm prefixes each provider puts in front of the hex digest.
const (
	slackPrefix  = slackVersion + "="
	githubPrefix = "sha256="

	// slackVersion is the signing-scheme version Slack prepends to the base
	// string it signs: "v0:<timestamp>:<body>".
	slackVersion = "v0"
)

// DefaultWindow is Slack's documented replay window.
const DefaultWindow = 5 * time.Minute

// Verifier authenticates a raw request body against its headers.
//
// The interface exists so that webhook plumbing never switches on provider: a
// route is configured with a Verifier and asks it one question. Adding a
// provider adds an implementation here and changes no handler.
//
// body must be the exact bytes read from the request, and Verify must be
// called before those bytes are parsed. Implementations return one of the
// sentinel errors above, wrapped with context, and nil only when the request
// is authentic.
type Verifier interface {
	Verify(header http.Header, body []byte) error
}

// Compile-time proof that both providers answer the same question, so the
// plumbing can hold either behind the interface.
var (
	_ Verifier = (*Slack)(nil)
	_ Verifier = (*GitHub)(nil)
)

// Slack verifies Slack Events API request signing:
//
//	X-Slack-Signature         = "v0=" + hex(HMAC_SHA256(secret, "v0:" + timestamp + ":" + body))
//	X-Slack-Request-Timestamp = the timestamp in that base string
//
// Because the timestamp is inside what is signed, it cannot be edited without
// invalidating the digest, which is what makes the replay window meaningful.
type Slack struct {
	// Secret is the Slack signing secret. A verifier with no secret refuses
	// every request with ErrNoSecret.
	Secret []byte

	// Window bounds how far the signed timestamp may sit from now, in either
	// direction: past for a replayed capture, future for a sender whose clock
	// runs ahead. Zero means DefaultWindow. A negative Window rejects
	// everything, which is the fail-closed reading of an absurd setting.
	Window time.Duration

	// Now is the clock seam. It is a field rather than a package global so
	// that two verifiers in one process can be tested independently, and so
	// that the replay window can be exercised without sleeping. Nil means
	// time.Now.
	Now func() time.Time
}

// NewSlack returns a verifier for a Slack signing secret, with the documented
// replay window and the real clock.
func NewSlack(secret string) *Slack { return &Slack{Secret: []byte(secret)} }

// Verify authenticates body against the Slack signature and timestamp headers.
//
// The timestamp is checked first and, when it is outside the window, the
// digest is not computed at all: a request that old is refused on its age
// alone.
func (s *Slack) Verify(header http.Header, body []byte) error {
	if len(s.Secret) == 0 {
		return fmt.Errorf("%w: slack verifier has an empty signing secret", ErrNoSecret)
	}

	timestamp, err := single(header, HeaderSlackTimestamp)
	if err != nil {
		return err
	}
	signature, err := single(header, HeaderSlackSignature)
	if err != nil {
		return err
	}
	want, err := digest(signature, slackPrefix, HeaderSlackSignature)
	if err != nil {
		return err
	}

	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: %s is not a unix timestamp: %q", ErrMalformed, HeaderSlackTimestamp, timestamp)
	}
	if err := s.checkSkew(time.Unix(seconds, 0)); err != nil {
		return err
	}

	// The timestamp goes into the base string exactly as it arrived, not as
	// the integer it parsed to: the sender signed the header's bytes, and
	// re-rendering them would break any encoding we did not anticipate.
	mac := hmac.New(sha256.New, s.Secret)
	_, _ = mac.Write([]byte(slackVersion))
	_, _ = mac.Write([]byte{':'})
	_, _ = mac.Write([]byte(timestamp))
	_, _ = mac.Write([]byte{':'})
	_, _ = mac.Write(body)

	// hmac.Equal, never == and never bytes.Equal: those stop at the first
	// differing byte, and the time they take leaks how long a prefix matched.
	// That leak is enough to forge a digest one byte at a time against an
	// endpoint an attacker can call repeatedly, which is exactly what a
	// public webhook endpoint is. hmac.Equal compares in constant time.
	if !hmac.Equal(mac.Sum(nil), want) {
		return fmt.Errorf("%w: %s does not match the body under the slack signing secret", ErrSignatureMismatch, HeaderSlackSignature)
	}
	return nil
}

// checkSkew enforces the replay window.
//
// The boundary is INCLUSIVE: a timestamp exactly Window away from now is
// accepted, and one tick beyond it is rejected. Inclusive is the choice that
// makes the constant readable — "five minutes" means five minutes is still
// fine — and the difference is one nanosecond of exposure against a defense
// measured in minutes.
//
// The window is expressed as two instant comparisons rather than as the
// absolute value of a Duration, because the obvious spelling of the latter
// fails OPEN. Duration is an int64 of nanoseconds and spans roughly ±292
// years; Time is not bounded by it. A timestamp beyond that range makes Sub
// saturate at the minimum Duration, and negating the minimum int64 overflows
// back to itself — it stays negative — so "abs(skew) > window" reads false and
// admits a timestamp hundreds of millions of years in the future. Comparing
// instants has no such edge: Before and After are total orders over Time.
func (s *Slack) checkSkew(signed time.Time) error {
	window := s.Window
	if window == 0 {
		window = DefaultWindow
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}

	// A negative Window admits nothing: the bounds cross, so every instant
	// fails one of them. That is the fail-closed reading of an absurd setting.
	at := now()
	if signed.Before(at.Add(-window)) || signed.After(at.Add(window)) {
		return fmt.Errorf("%w: signed at %s, now %s, window is %s", ErrTimestampSkew, signed.UTC().Format(time.RFC3339), at.UTC().Format(time.RFC3339), window)
	}
	return nil
}

// GitHub verifies GitHub webhook signing:
//
//	X-Hub-Signature-256 = "sha256=" + hex(HMAC_SHA256(secret, body))
//
// There is no timestamp in the base string, so there is no replay window to
// configure and none is offered. See the package doc: GitHub redelivery, and a
// replay of a captured request, are defended downstream by the event id
// idempotency key, not here.
type GitHub struct {
	// Secret is the webhook secret configured on the GitHub side. A verifier
	// with no secret refuses every request with ErrNoSecret.
	Secret []byte
}

// NewGitHub returns a verifier for a GitHub webhook secret.
func NewGitHub(secret string) *GitHub { return &GitHub{Secret: []byte(secret)} }

// Verify authenticates body against the GitHub signature header.
func (g *GitHub) Verify(header http.Header, body []byte) error {
	if len(g.Secret) == 0 {
		return fmt.Errorf("%w: github verifier has an empty webhook secret", ErrNoSecret)
	}

	signature, err := single(header, HeaderGitHubSignature)
	if err != nil {
		return err
	}
	want, err := digest(signature, githubPrefix, HeaderGitHubSignature)
	if err != nil {
		return err
	}

	mac := hmac.New(sha256.New, g.Secret)
	_, _ = mac.Write(body)

	// hmac.Equal for the same reason as in Slack.Verify: a comparison that
	// short-circuits leaks, through timing, how much of the digest was right,
	// and a public endpoint can be probed as many times as an attacker likes.
	if !hmac.Equal(mac.Sum(nil), want) {
		return fmt.Errorf("%w: %s does not match the body under the github webhook secret", ErrSignatureMismatch, HeaderGitHubSignature)
	}
	return nil
}

// single returns the one value of a header.
//
// A request carrying the header twice is rejected rather than resolved.
// Picking the first copy (or the last) is a decision, and any proxy, WAF, or
// framework in front of this code may have made the opposite one — the
// disagreement between two hops about which copy is "the" signature is the
// shape of a header-smuggling bug. Refusing costs nothing: no legitimate
// sender signs a request twice.
func single(header http.Header, key string) (string, error) {
	values := header.Values(key)
	switch {
	case len(values) == 0:
		return "", fmt.Errorf("%w: missing %s", ErrMalformed, key)
	case len(values) > 1:
		return "", fmt.Errorf("%w: %s appears %d times", ErrMalformed, key, len(values))
	case values[0] == "":
		return "", fmt.Errorf("%w: empty %s", ErrMalformed, key)
	}
	return values[0], nil
}

// digest strips the algorithm prefix from a header value and decodes the hex
// digest behind it.
//
// The length check is not redundant with hmac.Equal, which would also reject a
// short digest: it separates "this header is not a SHA-256 digest at all" from
// "this digest is wrong", so a truncated or misconfigured sender gets a 400
// and a forgery attempt gets a 401. Neither path may panic, so every slice is
// taken after its bound is established.
func digest(value, prefix, key string) ([]byte, error) {
	if !strings.HasPrefix(value, prefix) {
		return nil, fmt.Errorf("%w: %s is missing the %q prefix", ErrMalformed, key, prefix)
	}
	encoded := value[len(prefix):]
	if encoded == "" {
		return nil, fmt.Errorf("%w: %s carries no digest", ErrMalformed, key)
	}
	// hex.DecodeString rejects odd-length input and non-hex characters, so
	// neither reaches the comparison.
	decoded, err := hex.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("%w: %s digest is not hex: %w", ErrMalformed, key, err)
	}
	if len(decoded) != sha256.Size {
		return nil, fmt.Errorf("%w: %s digest is %d bytes, want %d", ErrMalformed, key, len(decoded), sha256.Size)
	}
	return decoded, nil
}
