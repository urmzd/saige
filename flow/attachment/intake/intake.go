// Package intake is how a file's bytes get into the blob store and come out
// as an attachment.Attachment. It is the one path every surface shares: a
// connector that already holds the bytes calls Store, and a connector that
// holds a URL calls Fetcher.Fetch, which downloads and then does the same.
//
// # What intake decides
//
// Everything about an attachment that should not be left to the uploader is
// decided here, once, at the door. The media type is sniffed from the content
// (see attachment.Sniff) and the claimed type is never consulted. The name is
// reduced to a display label. The size is the number of bytes actually read,
// bounded by a limit, not a Content-Length header. The digest is computed
// over those same bytes by the store. A connector supplies only what it alone
// knows: what the file was called, where it was attached and any alt text.
//
// # Fetching is a security boundary
//
// A URL in an issue body is attacker-controlled input that this process then
// dereferences from inside the deployment's network. Without care that is
// server-side request forgery: a link to http://169.254.169.254/ or to an
// internal admin port, fetched by us, with the response stored where an agent
// will read it back out. Fetcher is default deny on three independent axes,
// and a request must pass all of them:
//
//   - Host policy. A Fetcher is constructed with an Allow function and has no
//     default one. A connector names the hosts its surface serves files from.
//   - Network policy. The address actually dialed is checked at connect time,
//     after DNS resolution, and loopback, private, link-local and other
//     non-public ranges are refused. Checking the dialed address rather than
//     the hostname is what defeats a public name that resolves to a private
//     address, including one that changes its answer between a check and the
//     connection (DNS rebinding).
//   - Redirects. Each hop is re-validated against both policies, and
//     credentials are chosen per hop by the connector, so a bearer token for
//     one host is never replayed to wherever that host redirects.
//
// This package is stdlib plus pkg/blob, pkg/attachment and pkg/address.
package intake

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/attachment"
	"github.com/urmzd/saige/flow/blob"
)

// DefaultMaxBytes bounds one file when the caller names no limit. It admits a
// short screen recording or a long PDF and refuses anything that would take a
// meaningful share of a small deployment's disk in one upload.
const DefaultMaxBytes int64 = 25 << 20

// Meta is what the surface knows about a file that its bytes do not say.
type Meta struct {
	// Name is the file's name as the surface reported it. It is cleaned.
	Name string
	// Origin is where the file was attached.
	Origin address.Address
	// Alt is descriptive text supplied with the file. It is cleaned.
	Alt string
	// MaxBytes bounds the content. Zero means DefaultMaxBytes.
	MaxBytes int64
}

// Store reads r into the tenant's blob store and returns the attachment that
// refers to it. The media type is sniffed from the content. Storing the same
// content again returns a reference to the same blob.
//
// It fails with blob.ErrTooLarge if r exceeds the limit, in which case
// nothing is stored.
func Store(ctx context.Context, store blob.Store, tenant string, r io.Reader, meta Meta) (attachment.Attachment, error) {
	if store == nil {
		return attachment.Attachment{}, errors.New("intake: no blob store")
	}
	limit := meta.MaxBytes
	if limit <= 0 {
		limit = DefaultMaxBytes
	}
	// Read the sniffing prefix, then hand the store the prefix followed by
	// the rest, so the content is streamed once and never buffered whole.
	head := make([]byte, attachment.SniffLen)
	n, err := io.ReadFull(r, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return attachment.Attachment{}, fmt.Errorf("intake: read content: %w", err)
	}
	head = head[:n]
	name := attachment.CleanName(meta.Name)

	info, err := store.Put(ctx, tenant, io.MultiReader(newPrefix(head), r), blob.PutOptions{MaxBytes: limit})
	if err != nil {
		return attachment.Attachment{}, fmt.Errorf("intake: store %q: %w", name, err)
	}
	return attachment.Attachment{
		Digest:    info.Digest.String(),
		MediaType: attachment.Sniff(head, name),
		Name:      name,
		Size:      info.Size,
		Origin:    meta.Origin,
		Alt:       attachment.CleanAlt(meta.Alt),
	}, nil
}

// prefix replays the sniffed bytes ahead of the rest of the stream.
type prefix struct {
	b []byte
}

func newPrefix(b []byte) *prefix { return &prefix{b: b} }

func (p *prefix) Read(out []byte) (int, error) {
	if len(p.b) == 0 {
		return 0, io.EOF
	}
	n := copy(out, p.b)
	p.b = p.b[n:]
	return n, nil
}
