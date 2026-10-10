package types

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// PlaceholderVault swaps sensitive values in text for placeholders, as a
// privacy vault does.
type PlaceholderVault interface {
	Tokenize(ctx context.Context, text string) (string, error)
}

// Egress is the privacy boundary a request's conversions run inside. A
// privacy decorator sets it on the context of the calls it makes, and the
// conversion decorators below it apply it, so a converter's output never
// reaches the provider untokenized and media never leaves where the policy
// forbids it. The zero value sets no boundary.
type Egress struct {
	// Vault tokenizes the text a conversion puts into the view, before it
	// is substituted. It is also passed to converters as ConvertEnv.Vault.
	Vault PlaceholderVault
	// RequireText refuses a view that still carries opaque media after
	// conversion, and a converter that sends a part to a model whose
	// endpoint is not cleared for personal data (DataHandling.PIIOK).
	RequireText bool
	// Opaque reports whether a part is media the boundary cannot tokenize.
	// Nil counts every media part.
	Opaque func(Part) bool
}

// IsZero reports whether e sets no boundary.
func (e Egress) IsZero() bool { return e.Vault == nil && !e.RequireText }

// IsOpaque reports whether p is media the boundary cannot tokenize.
func (e Egress) IsOpaque(p Part) bool {
	if !IsMedia(p) {
		return false
	}
	return e.Opaque == nil || e.Opaque(p)
}

type egressKey struct{}

// WithEgress returns ctx carrying e for the conversion decorators of the
// providers called with it. The zero Egress clears a boundary set above.
func WithEgress(ctx context.Context, e Egress) context.Context {
	return context.WithValue(ctx, egressKey{}, e)
}

// EgressFrom returns the boundary ctx carries. It reports false when there
// is none.
func EgressFrom(ctx context.Context) (Egress, bool) {
	e, ok := ctx.Value(egressKey{}).(Egress)
	return e, ok && !e.IsZero()
}

// ErrUntrustedLocator reports a media locator an untrusted client may not
// supply: a vendor file ID, or a URI the provider or the host would resolve
// with the host's own credentials.
var ErrUntrustedLocator = errors.New("media locator not accepted from an untrusted client")

// vendorFileHosts are the hosts of vendor file stores. A URI on one of them
// names a file in the host's account, not public content.
var vendorFileHosts = []string{
	"generativelanguage.googleapis.com",
	"api.anthropic.com",
	"api.openai.com",
	"storage.googleapis.com",
	"storage.cloud.google.com",
}

// CheckClientSource reports whether s may come from an untrusted client,
// such as the body of a request to a server. Such a client may send inline
// bytes, a workspace reference (which resolves in its own session's
// workspace) and an http or https URL to public content. It may not send a
// vendor file, a URI with another scheme (gs, s3, file), a bare name such as
// "files/abc" or "file_abc", or a URL on a vendor's file store: each of
// those is resolved with the host's credentials and could reach another
// tenant's data. The error matches ErrUntrustedLocator.
func CheckClientSource(s Source) error {
	if len(s.Files) > 0 {
		return fmt.Errorf("%w: vendor file %q", ErrUntrustedLocator, s.Files[0].ID)
	}
	if s.URI == "" {
		return nil
	}
	u, err := url.Parse(s.URI)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%w: URI %q", ErrUntrustedLocator, s.URI)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return fmt.Errorf("%w: URI scheme %q", ErrUntrustedLocator, u.Scheme)
	}
	host := strings.ToLower(u.Hostname())
	for _, h := range vendorFileHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return fmt.Errorf("%w: URI on vendor file store %q", ErrUntrustedLocator, host)
		}
	}
	return nil
}

// CheckClientParts applies CheckClientSource to every media part of parts,
// including the parts of tool results. The error names the first part that
// fails.
func CheckClientParts[P Part](parts []P) error {
	for i, p := range parts {
		if src, ok := SourceOf(p); ok {
			if err := CheckClientSource(src); err != nil {
				return fmt.Errorf("part %d (%s): %w", i, p.Kind(), err)
			}
		}
		if tr, ok := any(p).(ToolResultPart); ok {
			for j, n := range tr.Parts {
				if src, ok := SourceOf(n); ok {
					if err := CheckClientSource(src); err != nil {
						return fmt.Errorf("part %d.%d (%s): %w", i, j, n.Kind(), err)
					}
				}
			}
		}
	}
	return nil
}
