// Package address is the naming layer of the mesh: a stable URI for any place
// a tag can come from or be delivered to.
//
//	github://urmzd/fiber/issues/42
//	slack://T0123/C0456?thread=1699123456.001
//	jira://acme/PROJ-5
//	cron://acme/nightly-review
//	grpc://acme/client-7
//
// The scheme is the connector name, the host is the workspace (an account
// boundary: a GitHub org, a Slack team ID, a Jira site), and the path locates
// a resource within it. Query parameters carry positioning that is not part of
// the resource's identity, such as a Slack thread timestamp.
//
// Workspace is constrained to subject-token characters because it becomes a
// subject token in the mesh delivery family. The path is not: issue numbers,
// thread timestamps, and Jira keys carry separators that would break subject
// matching, which is exactly why they travel in the address instead.
//
// This package is a leaf: stdlib only. It knows nothing about how any
// connector reaches its surface, only how to name it unambiguously.
package address

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// ErrInvalid reports a malformed address. Callers match with errors.Is.
var ErrInvalid = errors.New("address: invalid")

// Address names one endpoint in the mesh. The zero value is unset and
// reports true from IsZero.
type Address struct {
	// Connector is the scheme: which connector owns this endpoint.
	Connector string
	// Workspace is the account boundary. It is subject-token safe.
	Workspace string
	// Path locates a resource inside the workspace. Segments are stored
	// decoded and may contain any characters.
	Path []string
	// Params carry non-identity positioning (a Slack thread, a comment
	// durable). Nil when absent.
	Params map[string]string
}

// Parse reads an address URI. The scheme and host are required; the path and
// query are optional.
func Parse(s string) (Address, error) {
	if s == "" {
		return Address{}, fmt.Errorf("%w: empty address", ErrInvalid)
	}
	u, err := url.Parse(s)
	if err != nil {
		return Address{}, fmt.Errorf("%w: %q: %w", ErrInvalid, s, err)
	}
	if u.Scheme == "" {
		return Address{}, fmt.Errorf("%w: %q has no connector scheme", ErrInvalid, s)
	}
	// One rule for what a scheme is, enforced here rather than assumed. A
	// connector's name is the scheme of every address that reaches it, so
	// anything ValidScheme rejects must also be unparseable: otherwise an
	// address would exist that names a connector the registry refuses to
	// hold, and the failure would surface at delivery instead of at parse.
	//
	// The check is on the text as written, not on u.Scheme: net/url has
	// already lower-cased that one, so validating it would silently accept
	// "GitHub://" and hand back an address nobody wrote.
	if err := ValidScheme(s[:strings.IndexByte(s, ':')]); err != nil {
		return Address{}, fmt.Errorf("%w: in %q: %w", ErrInvalid, s, err)
	}
	if u.Host == "" {
		return Address{}, fmt.Errorf("%w: %q has no workspace", ErrInvalid, s)
	}
	if err := ValidWorkspace(u.Host); err != nil {
		return Address{}, fmt.Errorf("%w: in %q: %w", ErrInvalid, s, err)
	}

	a := Address{Connector: u.Scheme, Workspace: u.Host}
	// Split the still-escaped path, not u.Path. u.Path is already decoded, so
	// splitting it would break a segment at an encoded "/" and would then
	// unescape a second time, turning a segment containing "%" into a decode
	// error. Segments are the unit of identity here, and only the escaped form
	// still says where one ends.
	for _, seg := range strings.Split(strings.Trim(u.EscapedPath(), "/"), "/") {
		if seg == "" {
			continue
		}
		decoded, err := url.PathUnescape(seg)
		if err != nil {
			return Address{}, fmt.Errorf("%w: path segment %q in %q: %w", ErrInvalid, seg, s, err)
		}
		a.Path = append(a.Path, decoded)
	}
	if q := u.Query(); len(q) > 0 {
		a.Params = make(map[string]string, len(q))
		for k := range q {
			a.Params[k] = q.Get(k)
		}
	}
	return a, nil
}

// MustParse is Parse for constants and tests. It panics on a malformed
// address.
func MustParse(s string) Address {
	a, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return a
}

// String renders the address back to its URI form. Parameters are sorted so
// that the output is deterministic and an address can be used as a map key or
// compared across processes.
func (a Address) String() string {
	if a.IsZero() {
		return ""
	}
	var b strings.Builder
	b.WriteString(a.Connector)
	b.WriteString("://")
	b.WriteString(a.Workspace)
	for _, seg := range a.Path {
		b.WriteString("/")
		b.WriteString(url.PathEscape(seg))
	}
	if len(a.Params) > 0 {
		keys := make([]string, 0, len(a.Params))
		for k := range a.Params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		q := url.Values{}
		for _, k := range keys {
			q.Set(k, a.Params[k])
		}
		b.WriteString("?")
		b.WriteString(q.Encode())
	}
	return b.String()
}

// IsZero reports whether a is unset.
func (a Address) IsZero() bool { return a.Connector == "" && a.Workspace == "" }

// Param returns a query parameter and whether it was present.
func (a Address) Param(key string) (string, bool) {
	v, ok := a.Params[key]
	return v, ok
}

// WithParam returns a copy of a with key set to value, leaving a unchanged.
func (a Address) WithParam(key, value string) Address {
	out := a
	out.Path = append([]string(nil), a.Path...)
	out.Params = make(map[string]string, len(a.Params)+1)
	for k, v := range a.Params {
		out.Params[k] = v
	}
	out.Params[key] = value
	return out
}

// Resource joins the path segments with "/" — the connector-facing view of
// what this address points at, without scheme or workspace.
func (a Address) Resource() string { return strings.Join(a.Path, "/") }

// ValidScheme reports whether s is usable as a connector scheme.
//
// The rule is URI's, narrowed to lower case: a leading letter, then letters,
// digits, "+", "-" and ".". Narrowing matters because an address is compared
// and routed as text: if both "GitHub://acme/x" and "github://acme/x" parsed,
// one endpoint would have two spellings, and the router would open two
// delivery lanes onto one surface. Parse enforces this rule too, so the
// spelling that reaches a connector is the only one that exists.
//
// It is the address-level rule, not the registry's: connector.ValidName is
// stricter still, because a connector name has to survive being a token in
// places a URI scheme never goes.
func ValidScheme(s string) error {
	if s == "" {
		return fmt.Errorf("%w: empty scheme", ErrInvalid)
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case i == 0:
			return fmt.Errorf("%w: scheme %q must begin with a lower-case letter, not %q", ErrInvalid, s, r)
		case r >= '0' && r <= '9', r == '+', r == '-', r == '.':
		default:
			return fmt.Errorf("%w: scheme %q contains %q, which no URI scheme may carry", ErrInvalid, s, r)
		}
	}
	return nil
}

// ValidWorkspace reports whether w is usable as a subject token. The mesh
// delivery family embeds the workspace in its subject, so a workspace that
// cannot be a token cannot be sharded on and is rejected at parse time rather
// than failing later at publish.
func ValidWorkspace(w string) error {
	if w == "" {
		return fmt.Errorf("%w: empty workspace", ErrInvalid)
	}
	for _, r := range w {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_':
		default:
			return fmt.Errorf("%w: workspace %q contains %q, which is not subject-token safe", ErrInvalid, w, r)
		}
	}
	return nil
}
