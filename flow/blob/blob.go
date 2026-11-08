// Package blob is a content-addressed store for the bytes of files an agent
// is handed: a screenshot pasted into a Slack thread, a log attached to a
// GitHub issue, a PDF on a Jira ticket.
//
// # Why content addressing
//
// A blob's key is the SHA-256 of its content, and that one decision buys the
// three properties the rest of the system leans on:
//
//   - A reference is stable across replays. A durable run re-executes from the
//     top on every attempt, and its input must be byte-identical each time. A
//     digest names content, not a storage location or an upload event, so a
//     tag that carries one resolves to the same bytes on attempt five as on
//     attempt one, and the run's input never has to change.
//   - The same file is stored once. Every surface that raises tags redelivers
//     on timeout, and people attach the same screenshot to three threads.
//     Putting identical content twice is a no-op that returns the same digest.
//   - Integrity is checkable. A reader can recompute the digest of what it was
//     given, so a truncated or tampered blob is detected rather than handed to
//     a model as if it were the original.
//
// # Tenant scoping
//
// Tenant is an explicit argument on every method, and it is part of the key:
// a blob is addressed by (tenant, digest), never by digest alone. A digest is
// not a secret. It can be computed by anyone who has the file, and it travels
// on tags, in logs and in events. If knowing a digest were enough to read a
// blob, a tenant could probe for another tenant's files by guessing or
// replaying hashes. So a blob put by one tenant does not exist for any other,
// which also means identical content is deduplicated within a tenant and
// deliberately not across tenants: cross-tenant deduplication would turn
// "does this Put return quickly" into an oracle for "does someone else have
// this file".
//
// The caller supplies the tenant from the run's authorization scope. As
// everywhere else in flow, it is never read from a request body.
//
// # What this package is not
//
// It stores bytes and nothing about them: no media type, no filename, no
// origin. Those belong to pkg/attachment, which holds a digest and describes
// what the bytes are. Keeping the store ignorant is what lets one blob back
// two attachments with different names.
//
// # References
//
// Content addressing answers "give me these bytes" and cannot answer "give me
// what was computed from those bytes": a transcript's digest is not knowable
// from the recording's. A reference is the small missing piece, a tenant
// scoped name that points at a digest. pkg/attachment uses it to key a derived
// artifact by (source digest, ingestor, version), so a transcript is computed
// once and every replay of the run reads the same one. References are first
// writer wins for exactly that reason: two attempts racing to derive the same
// thing must agree afterwards on a single answer, or the run's prompt would
// differ between attempts.
//
// There is no garbage collection and no reference counting. Delete is
// explicit. A deployment that wants retention must decide when a run's blobs
// are no longer needed for replay, and that is a question about run lifetime
// this package cannot answer.
//
// This package is stdlib only.
package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
)

// DefaultMaxBytes bounds a Put that names no limit of its own. It is above
// what a model will accept as a single file and below anything that lets one
// upload exhaust a small deployment's disk or memory.
const DefaultMaxBytes int64 = 32 << 20

// Errors a Store returns. Callers match with errors.Is.
var (
	// ErrNotFound reports that the tenant has no blob with that digest, or
	// no reference with that key. A
	// blob held by a different tenant is reported the same way, so the answer
	// reveals nothing about other tenants.
	ErrNotFound = errors.New("blob: not found")
	// ErrTooLarge reports content longer than the Put's limit. Nothing is
	// stored.
	ErrTooLarge = errors.New("blob: too large")
	// ErrDigestMismatch reports content whose digest is not the one the
	// caller said to expect. Nothing is stored.
	ErrDigestMismatch = errors.New("blob: digest mismatch")
	// ErrInvalid reports a malformed digest or an empty tenant.
	ErrInvalid = errors.New("blob: invalid")
	// ErrCorrupt reports stored content that no longer matches its digest, or
	// a stored entry that is not a regular file.
	ErrCorrupt = errors.New("blob: corrupt")
)

// Digest is the lowercase hex SHA-256 of a blob's content. It is the whole
// key within a tenant. The canonical form is exactly 64 lowercase hex digits,
// so two digests are the same blob if and only if the strings are equal.
type Digest string

// ParseDigest validates s as a digest. Uppercase hex is rejected rather than
// folded, so that there is one spelling of each key.
func ParseDigest(s string) (Digest, error) {
	d := Digest(s)
	if err := d.Validate(); err != nil {
		return "", err
	}
	return d, nil
}

// Sum returns the digest of b.
func Sum(b []byte) Digest {
	sum := sha256.Sum256(b)
	return Digest(hex.EncodeToString(sum[:]))
}

// String returns the hex form.
func (d Digest) String() string { return string(d) }

// Validate reports whether d is a canonical digest. Every Store method calls
// it before touching storage, which is what makes a digest safe to use as a
// file name: a value that passes contains nothing but hex digits.
func (d Digest) Validate() error {
	if len(d) != sha256.Size*2 {
		return fmt.Errorf("%w: digest must be %d hex digits, got %d characters", ErrInvalid, sha256.Size*2, len(d))
	}
	for i := 0; i < len(d); i++ {
		c := d[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("%w: digest has a character outside lowercase hex at position %d", ErrInvalid, i)
		}
	}
	return nil
}

// Info describes a stored blob.
type Info struct {
	Digest Digest `json:"digest"`
	Size   int64  `json:"size"`
}

// PutOptions tunes one Put.
type PutOptions struct {
	// MaxBytes is the largest content accepted. Zero means DefaultMaxBytes.
	// There is no "unlimited": the content comes from an uploader.
	MaxBytes int64
	// Expect, when set, is the digest the content must have. It is how a
	// caller that was told a digest by someone else (a manifest, a peer)
	// refuses content that is not what was promised.
	Expect Digest
}

// Store is a tenant-scoped, content-addressed blob store.
//
// Implementations are safe for concurrent use. Run a new one through
// blobtest.Run, which holds every backend to the same contract.
type Store interface {
	// Put streams r into the store and returns the digest and size of what
	// was read. It reads at most the limit plus one byte, so an oversized
	// upload costs a bounded amount before it is refused. Content already
	// present for the tenant is not stored again. On any error nothing is
	// stored and nothing partial is left visible to Get.
	Put(ctx context.Context, tenant string, r io.Reader, opts PutOptions) (Info, error)

	// Get opens the tenant's blob for reading. The caller must close the
	// reader. A read may fail with ErrCorrupt if the stored content no longer
	// matches its digest, so a caller must treat the bytes as valid only once
	// it has read to io.EOF.
	Get(ctx context.Context, tenant string, d Digest) (io.ReadCloser, Info, error)

	// Stat reports the tenant's blob without reading it.
	Stat(ctx context.Context, tenant string, d Digest) (Info, error)

	// Delete removes the tenant's blob. Deleting a blob that is not there is
	// not an error: the caller wanted it gone and it is, which is what keeps
	// a retried cleanup from failing.
	Delete(ctx context.Context, tenant string, d Digest) error

	// PutRef points the tenant's reference key at d and returns the digest
	// the reference holds afterwards. The first writer wins: if key already
	// points at a blob the tenant still has, that digest is returned and d is
	// ignored. A reference whose blob has since been deleted is replaced. d
	// must be a blob the tenant has, or the result is ErrNotFound.
	PutRef(ctx context.Context, tenant, key string, d Digest) (Digest, error)

	// Ref returns the digest the tenant's reference key points at, or
	// ErrNotFound. It does not promise the blob is still there.
	Ref(ctx context.Context, tenant, key string) (Digest, error)

	// DeleteRef removes the tenant's reference. Like Delete, removing one
	// that is not there is not an error.
	DeleteRef(ctx context.Context, tenant, key string) error
}

// MaxRefKey bounds a reference key. Keys are structured names built by
// callers, not content, and nothing legitimate approaches this.
const MaxRefKey = 512

func checkRef(ctx context.Context, tenant, key string) error {
	if err := checkTenant(ctx, tenant); err != nil {
		return err
	}
	if key == "" || len(key) > MaxRefKey {
		return fmt.Errorf("%w: reference key must be 1 to %d bytes, got %d", ErrInvalid, MaxRefKey, len(key))
	}
	return nil
}

// check validates the arguments common to the keyed methods.
func check(ctx context.Context, tenant string, d Digest) error {
	if err := checkTenant(ctx, tenant); err != nil {
		return err
	}
	return d.Validate()
}

// checkTenant refuses an empty tenant. An empty scope is almost always a
// caller that lost the tenant on the way here, and silently filing those
// blobs under one shared key would join every such caller into one tenant.
func checkTenant(ctx context.Context, tenant string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if tenant == "" {
		return fmt.Errorf("%w: empty tenant", ErrInvalid)
	}
	return nil
}

// ingest copies r into w while hashing it, enforcing the size limit and the
// expected digest. It is the one place both rules live, so no backend can
// implement them differently.
func ingest(ctx context.Context, w io.Writer, r io.Reader, opts PutOptions) (Info, error) {
	limit := opts.MaxBytes
	if limit <= 0 {
		limit = DefaultMaxBytes
	}
	if opts.Expect != "" {
		if err := opts.Expect.Validate(); err != nil {
			return Info{}, fmt.Errorf("blob: expected digest: %w", err)
		}
	}
	h := sha256.New()
	// One byte past the limit is enough to know the content is too long
	// without reading the rest of it.
	n, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(&ctxReader{ctx: ctx, r: r}, limit+1))
	if err != nil {
		return Info{}, fmt.Errorf("blob: read content: %w", err)
	}
	if n > limit {
		return Info{}, fmt.Errorf("%w: content exceeds the %d byte limit", ErrTooLarge, limit)
	}
	got := digestOf(h)
	if opts.Expect != "" && got != opts.Expect {
		return Info{}, fmt.Errorf("%w: expected %s, content is %s", ErrDigestMismatch, opts.Expect, got)
	}
	return Info{Digest: got, Size: n}, nil
}

func digestOf(h hash.Hash) Digest { return Digest(hex.EncodeToString(h.Sum(nil))) }

// ctxReader stops a copy when its context ends, so a cancelled upload does
// not run to the size limit first.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
