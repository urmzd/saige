// Tests for the webhook authentication boundary.
//
// Two rules govern this file, and they are the reason it is longer than the
// package it tests.
//
// First, every expected digest is pinned as a literal computed OUTSIDE this
// package. A test that derives its expectation by calling the same code path it
// is testing proves only that the code is self-consistent; it would still pass
// if someone rewrote the signing base string. The literals below were produced
// by an independent program and they encode, byte for byte, that Slack signs
// "v0:" + timestamp + ":" + body and GitHub signs the raw body alone. Change
// either base string and these tests fail, which is the entire point.
//
// Second, every rejection is checked with errors.Is against the documented
// sentinel, never merely for "an error". The sentinels are the contract a
// handler translates into a status code, and a test that accepts any error
// would let ErrNoSecret (our misconfiguration, a 500) silently degrade into
// ErrSignatureMismatch (their forgery, a 401), or the reverse.
package signature_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/flow/signature"
)

const (
	slackSecret  = "8f742231b10e8888abcd99yyyzzz85a5"
	githubSecret = "It's a Secret to Everybody"

	// signedAt is the unix timestamp inside every Slack vector below. Tests
	// move the clock around it rather than moving it around the clock, so a
	// vector's digest stays valid while its age changes.
	signedAt   = int64(1700000000)
	timestamp  = "1700000000"
	zeroDigest = "0000000000000000000000000000000000000000000000000000000000000000"
)

// now is the fixed instant the signed timestamp sits exactly on.
func now() time.Time { return time.Unix(signedAt, 0) }

// Bodies. Each is a distinct byte shape: structured ASCII, nothing at all,
// multi-byte UTF-8 including an astral-plane rune, and all 256 byte values —
// the last of which is not valid UTF-8 anywhere and would be mangled by any
// implementation that treated the body as text.
var (
	bodyJSON    = []byte(`{"type":"event_callback","event":{"type":"app_mention"}}`)
	bodyEmpty   = []byte(``)
	bodyUnicode = []byte("{\"text\":\"héllo 世界 \U0001F30D — café\"}")
)

func bodyBinary() []byte {
	b := make([]byte, 256)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

// ---------------------------------------------------------------------------
// Independent reference implementation.
//
// Deliberately spelled differently from the package under test: one
// concatenated base string built with fmt, hashed in a single Write, instead of
// the package's sequence of five Writes. If the two ever disagree, one of them
// changed the bytes being signed.
// ---------------------------------------------------------------------------

func refSlack(secret, ts string, body []byte) string {
	base := append([]byte(fmt.Sprintf("v0:%s:", ts)), body...)
	return "v0=" + hexMAC(secret, base)
}

func refGitHub(secret string, body []byte) string {
	return "sha256=" + hexMAC(secret, body)
}

func hexMAC(secret string, base []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(base)
	return hex.EncodeToString(m.Sum(nil))
}

func slackHeader(ts, sig string) http.Header {
	h := http.Header{}
	h.Set(signature.HeaderSlackTimestamp, ts)
	h.Set(signature.HeaderSlackSignature, sig)
	return h
}

func githubHeader(sig string) http.Header {
	h := http.Header{}
	h.Set(signature.HeaderGitHubSignature, sig)
	return h
}

// slackAt returns a verifier whose clock reads at.
func slackAt(at time.Time) *signature.Slack {
	return &signature.Slack{
		Secret: []byte(slackSecret),
		Window: signature.DefaultWindow,
		Now:    func() time.Time { return at },
	}
}

func mustBe(t *testing.T, err, want error) {
	t.Helper()
	if err == nil {
		t.Fatalf("Verify() = nil, want %v", want)
	}
	if !errors.Is(err, want) {
		t.Fatalf("Verify() error = %v, want errors.Is(_, %v)", err, want)
	}
}

// ---------------------------------------------------------------------------
// Known-good vectors. The literals pin the base string.
// ---------------------------------------------------------------------------

func TestSlackAcceptsKnownGoodVectors(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		sig  string // hex(HMAC-SHA256(slackSecret, "v0:1700000000:" + body))
	}{
		{"json", bodyJSON, "v0=a26c279d1cb3398a35a4c509f20523b2c5e109184463f759ec2abd4017d47bf7"},
		{"empty body", bodyEmpty, "v0=e32114b39f9a22e4b8d8778478b39e6ed0677e5562e5474c3104b4074f1917e2"},
		{"unicode", bodyUnicode, "v0=eafaab9eeb84cdecf9eb9081f7a30a38e3620723bcf1a2d38682dcf7dca5937c"},
		{"all 256 byte values", bodyBinary(), "v0=5e6a5c6d2c468d7e26b10b01aa5bda8e81c8f7a3cc0cd464c014aab5db65eb60"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The pinned literal and the independent reference must agree;
			// if they diverge the vector has rotted and nothing below means
			// anything.
			if got := refSlack(slackSecret, timestamp, tc.body); got != tc.sig {
				t.Fatalf("reference signature = %s, pinned vector = %s", got, tc.sig)
			}
			if err := slackAt(now()).Verify(slackHeader(timestamp, tc.sig), tc.body); err != nil {
				t.Fatalf("Verify(valid %s) = %v, want nil", tc.name, err)
			}
		})
	}
}

func TestGitHubAcceptsKnownGoodVectors(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		sig  string // hex(HMAC-SHA256(githubSecret, body))
	}{
		{"json", bodyJSON, "sha256=8d11b710a16f7a8209fe18fbd0142d7a9561ad693c7631eb534ddcfe5367bd10"},
		{"empty body", bodyEmpty, "sha256=66a0c074deaa0f489ead6537e0d32f9a344b90bbeda705b6ed45ecd3b413fb40"},
		{"unicode", bodyUnicode, "sha256=67caff0aef5181dc9cdd304f902659b8014a0160f0d92aea7f0f3bc97dce983c"},
		{"all 256 byte values", bodyBinary(), "sha256=90ae8863901131b841e8cf5c484806df703b082fc7b3292552308ec3f3696956"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := refGitHub(githubSecret, tc.body); got != tc.sig {
				t.Fatalf("reference signature = %s, pinned vector = %s", got, tc.sig)
			}
			if err := signature.NewGitHub(githubSecret).Verify(githubHeader(tc.sig), tc.body); err != nil {
				t.Fatalf("Verify(valid %s) = %v, want nil", tc.name, err)
			}
		})
	}
}

// A nil body is not the same value as an empty slice in Go, but it is the same
// zero bytes on the wire, and io.ReadAll can hand a handler either one.
func TestNilBodyVerifiesAsEmptyBody(t *testing.T) {
	slackSig := "v0=e32114b39f9a22e4b8d8778478b39e6ed0677e5562e5474c3104b4074f1917e2"
	if err := slackAt(now()).Verify(slackHeader(timestamp, slackSig), nil); err != nil {
		t.Fatalf("Slack.Verify(nil body) = %v, want nil", err)
	}
	ghSig := "sha256=66a0c074deaa0f489ead6537e0d32f9a344b90bbeda705b6ed45ecd3b413fb40"
	if err := signature.NewGitHub(githubSecret).Verify(githubHeader(ghSig), nil); err != nil {
		t.Fatalf("GitHub.Verify(nil body) = %v, want nil", err)
	}
}

// ---------------------------------------------------------------------------
// The base string is exactly what the doc comment says it is.
//
// Every digest here is a well-formed HMAC under the correct secret over a
// PLAUSIBLE but wrong base string. If the implementation ever signed one of
// these instead, the vectors above would break and one of these would start
// passing, so the two tables pin the base string from both sides.
// ---------------------------------------------------------------------------

func TestSlackSigningBaseStringIsVersionTimestampBody(t *testing.T) {
	cases := []struct {
		name string
		sig  string
	}{
		{"body alone, as GitHub signs it", "v0=cd4e9e137795b5e38f7e7bcae069587c16d81251eb83466a8c0698879cf64daa"},
		{"timestamp and body without the version", "v0=6cfd3463e48a94f5f656dd9d38926efb3ff9124b22f40a44c52d337818b2ea7b"},
		{"version and body without the timestamp", "v0=768f098f74b30887693b77344267e7e903b4f17887904537106037ccb4e65d70"},
		{"all three concatenated without separators", "v0=c85214bfdae168b37f59b1b3b35a98a01e2deb144a0ddc9c9dc1e1865a06b5c5"},
		{"a v1 version string", "v0=92b8ffb7e02aca63a6e8ec037bf5f86fd897aaa1ac4a671df123e61cf3086d99"},
		{"a trailing separator after the body", "v0=9124ca080e81acd1c1212287fd55a8658320838ed5ecd833a9795a649a65947f"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := slackAt(now()).Verify(slackHeader(timestamp, tc.sig), bodyJSON)
			mustBe(t, err, signature.ErrSignatureMismatch)
		})
	}
}

func TestGitHubSigningBaseStringIsTheRawBodyAlone(t *testing.T) {
	cases := []struct {
		name string
		sig  string
	}{
		{"the algorithm prefix folded into the base string", "sha256=6230ca72c353d3284c369bad748c359de2b3757c71dc05542178b8bc24c5c9c3"},
		{"the Slack base string", "sha256=1b6d5d2902e7b8580330b6152c5052f31c2ea85c21616f7d4b37eb75afee213a"},
		{"a timestamp prepended to the body", "sha256=90d2f996578fabc5a2c56b49d55e8bb67933246ced6a9339d042174ef4acaa8c"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := signature.NewGitHub(githubSecret).Verify(githubHeader(tc.sig), bodyJSON)
			mustBe(t, err, signature.ErrSignatureMismatch)
		})
	}
}

// The timestamp is inside the Slack base string, so it cannot be edited without
// invalidating the digest. That is the property that makes the replay window
// mean anything at all: an attacker who could freshen the timestamp on a
// captured request would walk straight through the window check.
func TestSlackTimestampIsCoveredByTheSignature(t *testing.T) {
	sig := refSlack(slackSecret, timestamp, bodyJSON)

	t.Run("freshened timestamp", func(t *testing.T) {
		fresher := strconv.FormatInt(signedAt+60, 10)
		// The freshened timestamp is well inside the window, so the only
		// thing that can reject it is the digest.
		v := slackAt(time.Unix(signedAt+60, 0))
		mustBe(t, v.Verify(slackHeader(fresher, sig), bodyJSON), signature.ErrSignatureMismatch)
	})

	// The base string carries the header's BYTES, not the integer they parse
	// to. "+1700000000" is a perfectly legal ParseInt input for the same
	// instant, so it sails through the window check and then fails on the
	// digest — which is the proof that the package never re-renders the
	// timestamp it verifies.
	t.Run("numerically equal but textually different timestamp", func(t *testing.T) {
		mustBe(t, slackAt(now()).Verify(slackHeader("+"+timestamp, sig), bodyJSON), signature.ErrSignatureMismatch)
		// And the same request signed over those exact bytes is accepted.
		reSigned := refSlack(slackSecret, "+"+timestamp, bodyJSON)
		if err := slackAt(now()).Verify(slackHeader("+"+timestamp, reSigned), bodyJSON); err != nil {
			t.Fatalf("Verify(signed over the header bytes) = %v, want nil", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Forgery.
// ---------------------------------------------------------------------------

func TestSlackRejectsForgedRequests(t *testing.T) {
	valid := refSlack(slackSecret, timestamp, bodyJSON)

	cases := []struct {
		name string
		sig  string
		body []byte
	}{
		{"body tampered after signing", valid, append(append([]byte{}, bodyJSON...), ' ')},
		{"body truncated by one byte", valid, bodyJSON[:len(bodyJSON)-1]},
		{"body replaced entirely", valid, []byte(`{"type":"url_verification"}`)},
		{"single bit flipped in the digest", flipHexNibble(valid), bodyJSON},
		{"digest signed under a different secret", refSlack(slackSecret+"x", timestamp, bodyJSON), bodyJSON},
		{"digest signed under an empty secret", refSlack("", timestamp, bodyJSON), bodyJSON},
		{"digest for a different body", refSlack(slackSecret, timestamp, bodyUnicode), bodyJSON},
		{"all-zero digest", "v0=" + zeroDigest, bodyJSON},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := slackAt(now()).Verify(slackHeader(timestamp, tc.sig), tc.body)
			mustBe(t, err, signature.ErrSignatureMismatch)
		})
	}
}

func TestGitHubRejectsForgedRequests(t *testing.T) {
	valid := refGitHub(githubSecret, bodyJSON)

	cases := []struct {
		name string
		sig  string
		body []byte
	}{
		{"body tampered after signing", valid, append(append([]byte{}, bodyJSON...), ' ')},
		{"body truncated by one byte", valid, bodyJSON[:len(bodyJSON)-1]},
		{"single bit flipped in the digest", flipHexNibble(valid), bodyJSON},
		{"digest signed under a different secret", refGitHub(githubSecret+"x", bodyJSON), bodyJSON},
		{"digest signed under an empty secret", refGitHub("", bodyJSON), bodyJSON},
		{"digest for a different body", refGitHub(githubSecret, bodyUnicode), bodyJSON},
		{"all-zero digest", "sha256=" + zeroDigest, bodyJSON},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := signature.NewGitHub(githubSecret).Verify(githubHeader(tc.sig), tc.body)
			mustBe(t, err, signature.ErrSignatureMismatch)
		})
	}
}

// flipHexNibble changes the last hex character of a signature, which changes
// the digest it decodes to without changing its length or its hex-ness.
func flipHexNibble(sig string) string {
	last := sig[len(sig)-1]
	if last == '0' {
		return sig[:len(sig)-1] + "1"
	}
	return sig[:len(sig)-1] + "0"
}

// The two verifiers answer different questions and must not accept each
// other's evidence, even when they hold the same secret.
func TestVerifiersDoNotAcceptEachOthersSignatures(t *testing.T) {
	const shared = "shared-secret"

	slack := &signature.Slack{Secret: []byte(shared), Now: now}
	gh := &signature.GitHub{Secret: []byte(shared)}

	slackSig := refSlack(shared, timestamp, bodyJSON)
	ghSig := refGitHub(shared, bodyJSON)

	// The GitHub digest carried in a Slack header: the prefix already differs,
	// so this never reaches the comparison.
	mustBe(t, slack.Verify(slackHeader(timestamp, ghSig), bodyJSON), signature.ErrMalformed)
	// The Slack digest re-prefixed for GitHub: well formed, wrong base string.
	mustBe(t, gh.Verify(githubHeader("sha256="+strings.TrimPrefix(slackSig, "v0=")), bodyJSON), signature.ErrSignatureMismatch)
}

// ---------------------------------------------------------------------------
// Malformed evidence. Nothing here is a forgery attempt as far as we can tell,
// so it is a 400, not a 401, and the distinction is the sentinel.
// ---------------------------------------------------------------------------

func TestSlackRejectsMalformedHeaders(t *testing.T) {
	valid := refSlack(slackSecret, timestamp, bodyJSON)
	hexDigest := strings.TrimPrefix(valid, "v0=")

	cases := []struct {
		name   string
		header http.Header
	}{
		{"no headers at all", http.Header{}},
		{"missing signature", func() http.Header {
			h := http.Header{}
			h.Set(signature.HeaderSlackTimestamp, timestamp)
			return h
		}()},
		{"missing timestamp", func() http.Header {
			h := http.Header{}
			h.Set(signature.HeaderSlackSignature, valid)
			return h
		}()},
		{"empty signature", slackHeader(timestamp, "")},
		{"empty timestamp", slackHeader("", valid)},
		{"duplicated signature", func() http.Header {
			h := http.Header{}
			h.Set(signature.HeaderSlackTimestamp, timestamp)
			h.Add(signature.HeaderSlackSignature, valid)
			h.Add(signature.HeaderSlackSignature, valid)
			return h
		}()},
		{"duplicated timestamp, identical values", func() http.Header {
			h := http.Header{}
			h.Add(signature.HeaderSlackTimestamp, timestamp)
			h.Add(signature.HeaderSlackTimestamp, timestamp)
			h.Set(signature.HeaderSlackSignature, valid)
			return h
		}()},
		{"duplicated timestamp, smuggled second value", func() http.Header {
			h := http.Header{}
			h.Add(signature.HeaderSlackTimestamp, timestamp)
			h.Add(signature.HeaderSlackTimestamp, "1")
			h.Set(signature.HeaderSlackSignature, valid)
			return h
		}()},
		{"missing algorithm prefix", slackHeader(timestamp, hexDigest)},
		{"github algorithm prefix", slackHeader(timestamp, "sha256="+hexDigest)},
		{"wrong version prefix", slackHeader(timestamp, "v1="+hexDigest)},
		{"prefix with no separator", slackHeader(timestamp, "v0"+hexDigest)},
		{"prefix only, no digest", slackHeader(timestamp, "v0=")},
		{"leading space before the prefix", slackHeader(timestamp, " "+valid)},
		{"non-hex digest", slackHeader(timestamp, "v0="+strings.Repeat("z", 64))},
		{"one non-hex character in the digest", slackHeader(timestamp, "v0=g"+hexDigest[1:])},
		{"odd-length hex", slackHeader(timestamp, "v0="+hexDigest[:63])},
		{"truncated digest", slackHeader(timestamp, "v0="+hexDigest[:62])},
		{"single hex byte", slackHeader(timestamp, "v0=ab")},
		{"over-long digest", slackHeader(timestamp, "v0="+hexDigest+"ab")},
		{"digest with trailing whitespace", slackHeader(timestamp, valid+" ")},
		{"timestamp is not a number", slackHeader("not-a-timestamp", valid)},
		{"timestamp is a float", slackHeader("1700000000.5", valid)},
		{"timestamp overflows int64", slackHeader("9223372036854775808", valid)},
		{"timestamp underflows int64", slackHeader("-9223372036854775809", valid)},
		{"timestamp is hex", slackHeader("0x65500B80", valid)},
		{"timestamp has whitespace", slackHeader(" 1700000000", valid)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := slackAt(now()).Verify(tc.header, bodyJSON)
			mustBe(t, err, signature.ErrMalformed)
		})
	}
}

func TestGitHubRejectsMalformedHeaders(t *testing.T) {
	valid := refGitHub(githubSecret, bodyJSON)
	hexDigest := strings.TrimPrefix(valid, "sha256=")

	cases := []struct {
		name   string
		header http.Header
	}{
		{"no headers at all", http.Header{}},
		{"empty signature", githubHeader("")},
		{"duplicated signature, identical values", func() http.Header {
			h := http.Header{}
			h.Add(signature.HeaderGitHubSignature, valid)
			h.Add(signature.HeaderGitHubSignature, valid)
			return h
		}()},
		{"duplicated signature, smuggled second value", func() http.Header {
			h := http.Header{}
			h.Add(signature.HeaderGitHubSignature, valid)
			h.Add(signature.HeaderGitHubSignature, "sha256="+zeroDigest)
			return h
		}()},
		{"missing algorithm prefix", githubHeader(hexDigest)},
		{"sha1 algorithm prefix", githubHeader("sha1=" + hexDigest)},
		{"slack version prefix", githubHeader("v0=" + hexDigest)},
		{"prefix with no separator", githubHeader("sha256" + hexDigest)},
		{"prefix only, no digest", githubHeader("sha256=")},
		{"leading space before the prefix", githubHeader(" " + valid)},
		{"non-hex digest", githubHeader("sha256=" + strings.Repeat("z", 64))},
		{"one non-hex character in the digest", githubHeader("sha256=g" + hexDigest[1:])},
		{"odd-length hex", githubHeader("sha256=" + hexDigest[:63])},
		{"truncated digest", githubHeader("sha256=" + hexDigest[:62])},
		{"single hex byte", githubHeader("sha256=ab")},
		{"over-long digest", githubHeader("sha256=" + hexDigest + "ab")},
		{"sha1-length digest", githubHeader("sha256=" + hexDigest[:40])},
		{"digest with trailing whitespace", githubHeader(valid + " ")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := signature.NewGitHub(githubSecret).Verify(tc.header, bodyJSON)
			mustBe(t, err, signature.ErrMalformed)
		})
	}
}

// hex.DecodeString accepts either case, so an uppercase digest decodes to the
// same bytes and verifies. Pinned because it is a real behavioural choice: the
// comparison is over decoded bytes, not over header text, and nothing here
// should ever start comparing strings.
func TestDigestIsComparedAsBytesNotText(t *testing.T) {
	slackSig := strings.ToUpper(strings.TrimPrefix(refSlack(slackSecret, timestamp, bodyJSON), "v0="))
	if err := slackAt(now()).Verify(slackHeader(timestamp, "v0="+slackSig), bodyJSON); err != nil {
		t.Fatalf("Slack.Verify(uppercase hex) = %v, want nil", err)
	}
	ghSig := strings.ToUpper(strings.TrimPrefix(refGitHub(githubSecret, bodyJSON), "sha256="))
	if err := signature.NewGitHub(githubSecret).Verify(githubHeader("sha256="+ghSig), bodyJSON); err != nil {
		t.Fatalf("GitHub.Verify(uppercase hex) = %v, want nil", err)
	}
}

// ---------------------------------------------------------------------------
// Misconfiguration. An empty secret must never read as "allow", because HMAC
// under an empty key is computable by anyone who knows the algorithm — which is
// everyone. It is also not a mismatch: the request may be perfectly authentic
// and we are the broken party, so it is a 500 and not a 401.
// ---------------------------------------------------------------------------

func TestEmptySecretIsErrNoSecret(t *testing.T) {
	slackSig := refSlack("", timestamp, bodyJSON)
	ghSig := refGitHub("", bodyJSON)

	t.Run("slack, nil secret", func(t *testing.T) {
		v := &signature.Slack{Now: now}
		mustBe(t, v.Verify(slackHeader(timestamp, slackSig), bodyJSON), signature.ErrNoSecret)
	})
	t.Run("slack, empty secret", func(t *testing.T) {
		v := &signature.Slack{Secret: []byte{}, Now: now}
		mustBe(t, v.Verify(slackHeader(timestamp, slackSig), bodyJSON), signature.ErrNoSecret)
	})
	t.Run("slack, NewSlack with an empty string", func(t *testing.T) {
		mustBe(t, signature.NewSlack("").Verify(slackHeader(timestamp, slackSig), bodyJSON), signature.ErrNoSecret)
	})
	t.Run("github, nil secret", func(t *testing.T) {
		v := &signature.GitHub{}
		mustBe(t, v.Verify(githubHeader(ghSig), bodyJSON), signature.ErrNoSecret)
	})
	t.Run("github, empty secret", func(t *testing.T) {
		v := &signature.GitHub{Secret: []byte{}}
		mustBe(t, v.Verify(githubHeader(ghSig), bodyJSON), signature.ErrNoSecret)
	})
	t.Run("github, NewGitHub with an empty string", func(t *testing.T) {
		mustBe(t, signature.NewGitHub("").Verify(githubHeader(ghSig), bodyJSON), signature.ErrNoSecret)
	})
}

// A misconfigured verifier reports the misconfiguration even when the request
// is also garbage. Reporting ErrMalformed here would blame the sender for our
// bug and hide an unconfigured secret behind a stream of 400s.
func TestEmptySecretIsReportedBeforeInspectingHeaders(t *testing.T) {
	mustBe(t, signature.NewSlack("").Verify(http.Header{}, bodyJSON), signature.ErrNoSecret)
	mustBe(t, signature.NewGitHub("").Verify(http.Header{}, bodyJSON), signature.ErrNoSecret)

	// Including the case where the headers are so bad they would otherwise be
	// rejected before any digest was computed.
	bad := slackHeader("not-a-timestamp", "garbage")
	mustBe(t, signature.NewSlack("").Verify(bad, bodyJSON), signature.ErrNoSecret)
}

// ---------------------------------------------------------------------------
// No SHA-1 downgrade.
// ---------------------------------------------------------------------------

// GitHub still sends X-Hub-Signature (SHA-1) alongside X-Hub-Signature-256.
// Honouring it would hand an attacker who can strip one header a downgrade to
// a broken hash, so a request carrying only the SHA-1 header is not
// authenticated — it is malformed, because the evidence this package requires
// is absent.
func TestGitHubNeverFallsBackToSHA1(t *testing.T) {
	// A genuine SHA-1 signature over bodyJSON under githubSecret, exactly as
	// GitHub computes the legacy header. It is correct, and it is ignored.
	sha1Sig := "sha1=c41e2754ed9b819d2be2f7cfc6752c5887bb3181"

	t.Run("only the sha1 header is present", func(t *testing.T) {
		h := http.Header{}
		h.Set("X-Hub-Signature", sha1Sig)
		mustBe(t, signature.NewGitHub(githubSecret).Verify(h, bodyJSON), signature.ErrMalformed)
	})

	t.Run("the sha1 header is not read even when it is valid", func(t *testing.T) {
		h := http.Header{}
		h.Set("X-Hub-Signature", sha1Sig)
		h.Set(signature.HeaderGitHubSignature, "sha256="+zeroDigest)
		mustBe(t, signature.NewGitHub(githubSecret).Verify(h, bodyJSON), signature.ErrSignatureMismatch)
	})

	t.Run("a sha1 digest smuggled into the sha256 header", func(t *testing.T) {
		// 20 bytes of hex is not a SHA-256 digest, whatever header it arrives
		// in, and the length check is what says so.
		h := githubHeader("sha256=" + strings.TrimPrefix(sha1Sig, "sha1="))
		mustBe(t, signature.NewGitHub(githubSecret).Verify(h, bodyJSON), signature.ErrMalformed)
	})

	t.Run("the sha256 header still decides when both are present", func(t *testing.T) {
		h := http.Header{}
		h.Set("X-Hub-Signature", "sha1="+strings.Repeat("0", 40))
		h.Set(signature.HeaderGitHubSignature, refGitHub(githubSecret, bodyJSON))
		if err := signature.NewGitHub(githubSecret).Verify(h, bodyJSON); err != nil {
			t.Fatalf("Verify() = %v, want nil", err)
		}
	})
}

// ---------------------------------------------------------------------------
// The replay window.
// ---------------------------------------------------------------------------

// The window is documented as inclusive at both edges, and the tests move the
// clock rather than the timestamp so that one signature stays valid throughout
// and age is the only variable.
func TestSlackReplayWindowBoundaryIsInclusive(t *testing.T) {
	sig := refSlack(slackSecret, timestamp, bodyJSON)
	const window = 5 * time.Minute

	cases := []struct {
		name    string
		clockAt time.Time
		want    error // nil means accepted
	}{
		{"signed exactly now", now(), nil},
		{"one nanosecond old", now().Add(time.Nanosecond), nil},
		{"one nanosecond in the future", now().Add(-time.Nanosecond), nil},
		{"exactly the window old", now().Add(window), nil},
		{"one nanosecond past the window", now().Add(window + time.Nanosecond), signature.ErrTimestampSkew},
		{"one second past the window", now().Add(window + time.Second), signature.ErrTimestampSkew},
		{"exactly the window in the future", now().Add(-window), nil},
		{"one nanosecond past the future window", now().Add(-window - time.Nanosecond), signature.ErrTimestampSkew},
		{"one second past the future window", now().Add(-window - time.Second), signature.ErrTimestampSkew},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := &signature.Slack{
				Secret: []byte(slackSecret),
				Window: window,
				Now:    func() time.Time { return tc.clockAt },
			}
			err := v.Verify(slackHeader(timestamp, sig), bodyJSON)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("Verify() = %v, want nil", err)
				}
				return
			}
			mustBe(t, err, tc.want)
		})
	}
}

// A captured request that is hours old is refused whatever its digest says,
// and so is one from a sender whose clock runs hours ahead.
func TestSlackRejectsStaleAndFutureTimestamps(t *testing.T) {
	sig := refSlack(slackSecret, timestamp, bodyJSON)

	cases := []struct {
		name   string
		offset time.Duration // clock offset from the signed instant
	}{
		{"an hour stale", time.Hour},
		{"a day stale", 24 * time.Hour},
		{"a year stale", 365 * 24 * time.Hour},
		{"an hour in the future", -time.Hour},
		{"a day in the future", -24 * time.Hour},
		{"a year in the future", -365 * 24 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := slackAt(now().Add(tc.offset)).Verify(slackHeader(timestamp, sig), bodyJSON)
			mustBe(t, err, signature.ErrTimestampSkew)
		})
	}
}

// Zero is not "no window". It means the documented five minutes, and the test
// pins both that the default is applied and that it is bounded.
func TestSlackZeroWindowUsesDefaultWindow(t *testing.T) {
	sig := refSlack(slackSecret, timestamp, bodyJSON)

	verifierAt := func(at time.Time) *signature.Slack {
		return &signature.Slack{Secret: []byte(slackSecret), Now: func() time.Time { return at }}
	}

	if signature.DefaultWindow != 5*time.Minute {
		t.Fatalf("DefaultWindow = %v, want 5m", signature.DefaultWindow)
	}
	if err := verifierAt(now().Add(signature.DefaultWindow)).Verify(slackHeader(timestamp, sig), bodyJSON); err != nil {
		t.Fatalf("Verify() at exactly DefaultWindow = %v, want nil", err)
	}
	if err := verifierAt(now().Add(-signature.DefaultWindow)).Verify(slackHeader(timestamp, sig), bodyJSON); err != nil {
		t.Fatalf("Verify() at exactly -DefaultWindow = %v, want nil", err)
	}
	mustBe(t,
		verifierAt(now().Add(signature.DefaultWindow+time.Nanosecond)).Verify(slackHeader(timestamp, sig), bodyJSON),
		signature.ErrTimestampSkew)
	mustBe(t,
		verifierAt(now().Add(-signature.DefaultWindow-time.Nanosecond)).Verify(slackHeader(timestamp, sig), bodyJSON),
		signature.ErrTimestampSkew)
}

// A negative window is nonsense, and the fail-closed reading of nonsense is
// "admit nothing". The bounds cross, so every instant — including the exact
// instant of signing — fails one of them.
func TestSlackNegativeWindowRejectsEverything(t *testing.T) {
	sig := refSlack(slackSecret, timestamp, bodyJSON)

	for _, window := range []time.Duration{-time.Nanosecond, -time.Second, -5 * time.Minute, -time.Hour} {
		t.Run(window.String(), func(t *testing.T) {
			for _, offset := range []time.Duration{0, time.Nanosecond, -time.Nanosecond, time.Minute, -time.Minute} {
				at := now().Add(offset)
				v := &signature.Slack{
					Secret: []byte(slackSecret),
					Window: window,
					Now:    func() time.Time { return at },
				}
				err := v.Verify(slackHeader(timestamp, sig), bodyJSON)
				mustBe(t, err, signature.ErrTimestampSkew)
			}
		})
	}
}

// The subtlest property in the package.
//
// time.Duration is an int64 of nanoseconds and saturates at roughly ±292 years,
// while time.Time is not bounded by it. The obvious skew check —
//
//	skew := signed.Sub(now); if skew < 0 { skew = -skew }; if skew > window {...}
//
// fails OPEN for a timestamp far enough in the PAST: Sub saturates at
// math.MinInt64, and negating math.MinInt64 overflows back to itself, so the
// "absolute value" stays negative, compares as less than the window, and the
// request is admitted. Comparing instants has no such edge, and this test both
// demonstrates the trap and proves the package does not fall into it.
func TestSlackSkewIsOverflowSafe(t *testing.T) {
	cases := []struct {
		name    string
		seconds int64
	}{
		{"year 5000", time.Date(5000, 1, 1, 0, 0, 0, 0, time.UTC).Unix()},
		{"year -5000", time.Date(-5000, 1, 1, 0, 0, 0, 0, time.UTC).Unix()},
		{"a billion years hence", 31557600000000000},
		{"a billion years past", -31557600000000000},
		{"math.MaxInt64 seconds", math.MaxInt64},
		{"math.MinInt64 seconds", math.MinInt64},
		{"the unix epoch", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := strconv.FormatInt(tc.seconds, 10)
			// Signed correctly for that timestamp, so the ONLY thing that can
			// reject it is the window.
			sig := refSlack(slackSecret, ts, bodyJSON)
			err := slackAt(now()).Verify(slackHeader(ts, sig), bodyJSON)
			mustBe(t, err, signature.ErrTimestampSkew)
		})
	}

	// The trap itself, demonstrated rather than asserted about: if the package
	// spelled the check this way, the year -5000 case above would be admitted.
	t.Run("the naive absolute-value spelling would fail open", func(t *testing.T) {
		signed := time.Date(-5000, 1, 1, 0, 0, 0, 0, time.UTC)
		skew := signed.Sub(now())
		if skew != math.MinInt64 {
			t.Fatalf("Sub did not saturate: got %d, want %d", int64(skew), int64(math.MinInt64))
		}
		if naive := -skew; naive != skew {
			t.Fatalf("negating math.MinInt64 did not overflow back to itself: got %d", int64(naive))
		}
		if naive := -skew; naive > signature.DefaultWindow {
			t.Fatalf("the naive check unexpectedly rejects; this test no longer demonstrates the trap")
		}
		// It reads as "in window". The package must not agree.
	})
}

// A request that is too old is refused on its age alone, without the digest
// ever being computed. This is not a micro-optimisation: it is what "replay
// window" means. The proof is that a stale request carrying an obviously wrong
// digest comes back as ErrTimestampSkew and not ErrSignatureMismatch.
func TestSlackStaleTimestampIsRejectedWithoutCheckingTheDigest(t *testing.T) {
	stale := slackAt(now().Add(time.Hour))

	cases := []struct {
		name string
		sig  string
	}{
		{"all-zero digest", "v0=" + zeroDigest},
		{"digest for a different body", refSlack(slackSecret, timestamp, bodyUnicode)},
		{"digest under a different secret", refSlack("attacker", timestamp, bodyJSON)},
		{"digest under an empty secret", refSlack("", timestamp, bodyJSON)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := stale.Verify(slackHeader(timestamp, tc.sig), bodyJSON)
			mustBe(t, err, signature.ErrTimestampSkew)
			if errors.Is(err, signature.ErrSignatureMismatch) {
				t.Fatalf("Verify() also reports a mismatch (%v); the digest was checked", err)
			}
		})
	}
}

// Ordering the other way: a header that cannot be interpreted is malformed
// before it can be stale. There is no timestamp to be stale against if the
// signature header is not a digest, so ErrMalformed wins.
func TestSlackMalformedSignatureOutranksStaleTimestamp(t *testing.T) {
	stale := slackAt(now().Add(time.Hour))
	mustBe(t, stale.Verify(slackHeader(timestamp, "garbage"), bodyJSON), signature.ErrMalformed)
	mustBe(t, stale.Verify(slackHeader("not-a-number", "v0="+zeroDigest), bodyJSON), signature.ErrMalformed)
}

// ---------------------------------------------------------------------------
// Immutability.
// ---------------------------------------------------------------------------

// Verifiers are documented as safe for concurrent use. Under -race this pins
// that Verify keeps no mutable state — in particular that the hmac.Hash is
// constructed per call and never shared, which is exactly the kind of
// "optimisation" that would silently corrupt digests under load.
func TestVerifiersAreSafeForConcurrentUse(t *testing.T) {
	slack := slackAt(now())
	gh := signature.NewGitHub(githubSecret)

	slackSig := refSlack(slackSecret, timestamp, bodyJSON)
	ghSig := refGitHub(githubSecret, bodyJSON)

	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				if err := slack.Verify(slackHeader(timestamp, slackSig), bodyJSON); err != nil {
					t.Errorf("Slack.Verify(valid) = %v, want nil", err)
				}
				if err := gh.Verify(githubHeader(ghSig), bodyJSON); err != nil {
					t.Errorf("GitHub.Verify(valid) = %v, want nil", err)
				}
				return
			}
			if err := slack.Verify(slackHeader(timestamp, "v0="+zeroDigest), bodyJSON); !errors.Is(err, signature.ErrSignatureMismatch) {
				t.Errorf("Slack.Verify(forged) = %v, want ErrSignatureMismatch", err)
			}
			if err := gh.Verify(githubHeader("sha256="+zeroDigest), bodyJSON); !errors.Is(err, signature.ErrSignatureMismatch) {
				t.Errorf("GitHub.Verify(forged) = %v, want ErrSignatureMismatch", err)
			}
		}(i)
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// Fuzzing.
//
// Two invariants, and the second is the one that matters: no input panics
// (a panic in a webhook handler is a denial of service reachable by anyone who
// knows the URL), and nil is returned ONLY for input that an independent
// reimplementation also considers authentic. A verifier that returned nil on
// some exotic header would be a total authentication bypass, and it would not
// look like a crash.
// ---------------------------------------------------------------------------

// slackAuthentic is the reference answer, written from the doc comment rather
// than from the implementation.
func slackAuthentic(at time.Time, window time.Duration, secret, ts, sig string, body []byte) bool {
	if secret == "" {
		return false
	}
	if !strings.HasPrefix(sig, "v0=") {
		return false
	}
	want, err := hex.DecodeString(sig[len("v0="):])
	if err != nil || len(want) != sha256.Size {
		return false
	}
	seconds, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	signed := time.Unix(seconds, 0)
	if signed.Before(at.Add(-window)) || signed.After(at.Add(window)) {
		return false
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(append([]byte(fmt.Sprintf("v0:%s:", ts)), body...))
	return hmac.Equal(m.Sum(nil), want)
}

func githubAuthentic(secret, sig string, body []byte) bool {
	if secret == "" {
		return false
	}
	if !strings.HasPrefix(sig, "sha256=") {
		return false
	}
	want, err := hex.DecodeString(sig[len("sha256="):])
	if err != nil || len(want) != sha256.Size {
		return false
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hmac.Equal(m.Sum(nil), want)
}

// isSentinel reports whether err carries one of the four documented sentinels.
// An error outside that set has no status code to map to.
func isSentinel(err error) bool {
	return errors.Is(err, signature.ErrSignatureMismatch) ||
		errors.Is(err, signature.ErrTimestampSkew) ||
		errors.Is(err, signature.ErrMalformed) ||
		errors.Is(err, signature.ErrNoSecret)
}

func FuzzSlackVerify(f *testing.F) {
	valid := refSlack(slackSecret, timestamp, bodyJSON)
	f.Add(timestamp, valid, bodyJSON)
	f.Add(timestamp, refSlack(slackSecret, timestamp, bodyEmpty), bodyEmpty)
	f.Add(timestamp, refSlack(slackSecret, timestamp, bodyUnicode), bodyUnicode)
	f.Add(timestamp, refSlack(slackSecret, timestamp, bodyBinary()), bodyBinary())
	f.Add(timestamp, strings.ToUpper(valid), bodyJSON)
	f.Add("", "", []byte(nil))
	f.Add(timestamp, "v0=", bodyJSON)
	f.Add(timestamp, "v0", bodyJSON)
	f.Add(timestamp, "v0=zz", bodyJSON)
	f.Add(timestamp, "v0="+zeroDigest, bodyJSON)
	f.Add("9223372036854775807", valid, bodyJSON)
	f.Add("-9223372036854775808", valid, bodyJSON)
	f.Add("95617584000", refSlack(slackSecret, "95617584000", bodyJSON), bodyJSON)
	f.Add("-219951936000", refSlack(slackSecret, "-219951936000", bodyJSON), bodyJSON)
	f.Add("\x00", "\x00", []byte("\x00"))

	at := now()
	const window = signature.DefaultWindow
	v := &signature.Slack{
		Secret: []byte(slackSecret),
		Window: window,
		Now:    func() time.Time { return at },
	}

	f.Fuzz(func(t *testing.T, ts, sig string, body []byte) {
		err := v.Verify(slackHeader(ts, sig), body)
		want := slackAuthentic(at, window, slackSecret, ts, sig, body)
		switch {
		case err == nil && !want:
			t.Fatalf("Verify(ts=%q, sig=%q, body=%q) = nil, but the request is not authentic", ts, sig, body)
		case err != nil && want:
			t.Fatalf("Verify(ts=%q, sig=%q, body=%q) = %v, but the request is authentic", ts, sig, body, err)
		case err != nil && !isSentinel(err):
			t.Fatalf("Verify(ts=%q, sig=%q) = %v, which is not one of the documented sentinels", ts, sig, err)
		}
	})
}

func FuzzGitHubVerify(f *testing.F) {
	valid := refGitHub(githubSecret, bodyJSON)
	f.Add(valid, bodyJSON)
	f.Add(refGitHub(githubSecret, bodyEmpty), bodyEmpty)
	f.Add(refGitHub(githubSecret, bodyUnicode), bodyUnicode)
	f.Add(refGitHub(githubSecret, bodyBinary()), bodyBinary())
	f.Add(strings.ToUpper(valid), bodyJSON)
	f.Add("", []byte(nil))
	f.Add("sha256=", bodyJSON)
	f.Add("sha256", bodyJSON)
	f.Add("sha256=zz", bodyJSON)
	f.Add("sha256="+zeroDigest, bodyJSON)
	f.Add("sha1=d03207e4b030cf234e3447bac4d93add4c6643d8", bodyJSON)
	f.Add("v0="+strings.TrimPrefix(valid, "sha256="), bodyJSON)
	f.Add("\x00", []byte("\x00"))

	v := signature.NewGitHub(githubSecret)

	f.Fuzz(func(t *testing.T, sig string, body []byte) {
		err := v.Verify(githubHeader(sig), body)
		want := githubAuthentic(githubSecret, sig, body)
		switch {
		case err == nil && !want:
			t.Fatalf("Verify(sig=%q, body=%q) = nil, but the request is not authentic", sig, body)
		case err != nil && want:
			t.Fatalf("Verify(sig=%q, body=%q) = %v, but the request is authentic", sig, body, err)
		case err != nil && !isSentinel(err):
			t.Fatalf("Verify(sig=%q) = %v, which is not one of the documented sentinels", sig, err)
		}
	})
}
