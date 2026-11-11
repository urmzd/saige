package intake

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/urmzd/saige/flow/attachment"
	"github.com/urmzd/saige/flow/blob"
)

// Errors a Fetcher returns. Callers match with errors.Is.
var (
	// ErrNotAllowed reports a URL the host policy refused, or one that is
	// not fetchable at all: a non-HTTPS scheme, embedded credentials, no
	// host. It is a decision about the URL, made before anything is sent.
	ErrNotAllowed = errors.New("intake: url not allowed")
	// ErrBlockedAddress reports a connection refused because the address it
	// would have reached is not a public one. This is the SSRF guard.
	ErrBlockedAddress = errors.New("intake: address not allowed")
	// ErrStatus reports a response other than 200.
	ErrStatus = errors.New("intake: unexpected status")
	// ErrTooManyRedirects reports a redirect chain longer than the limit.
	ErrTooManyRedirects = errors.New("intake: too many redirects")
)

// StatusError is a response other than 200 or a redirect. It matches
// ErrStatus, and carries the code so a connector can tell a refusal (401,
// 403) from a file that is not there. It names the host and never the URL.
type StatusError struct {
	Code int
	Host string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s: %d from %s", ErrStatus, e.Code, e.Host)
}

// Is makes a StatusError match ErrStatus.
func (e *StatusError) Is(target error) bool { return target == ErrStatus }

// Refused reports a status that means the credentials were not accepted.
func (e *StatusError) Refused() bool {
	return e.Code == http.StatusUnauthorized || e.Code == http.StatusForbidden
}

// Fetch defaults.
const (
	// DefaultTimeout bounds a whole fetch: resolution, connection, every
	// redirect and the body.
	DefaultTimeout = 30 * time.Second
	// DefaultMaxRedirects bounds a redirect chain. GitHub's attachment URLs
	// redirect once to signed storage; a few hops of slack is plenty.
	DefaultMaxRedirects = 5
)

// Allow is a host policy: it returns nil if u may be requested. It is called
// for the initial URL and again for every redirect target, before any request
// to that URL is made. Wrap ErrNotAllowed in what it returns.
type Allow func(u *url.URL) error

// Hosts returns a policy admitting exactly the listed hosts. A pattern is a
// host name, or "*.example.com" for any subdomain of example.com (not
// example.com itself). Matching is on the whole host, case-insensitively, so
// "github.com.evil.example" does not match "github.com". With no patterns it
// admits nothing.
func Hosts(patterns ...string) Allow {
	exact := make(map[string]bool)
	var suffixes []string
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(p), "."))
		if rest, ok := strings.CutPrefix(p, "*."); ok && rest != "" {
			suffixes = append(suffixes, "."+rest)
		} else if p != "" {
			exact[p] = true
		}
	}
	return func(u *url.URL) error {
		host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
		if exact[host] {
			return nil
		}
		for _, s := range suffixes {
			if strings.HasSuffix(host, s) && len(host) > len(s) {
				return nil
			}
		}
		return fmt.Errorf("%w: host %q is not on the allowlist", ErrNotAllowed, host)
	}
}

// AnyPublicHost admits every host name. The network policy still applies, so
// this means "anywhere on the public internet", which is the right policy for
// a surface whose users paste arbitrary links and the wrong one for a
// connector that knows where its files live.
func AnyPublicHost(*url.URL) error { return nil }

// NetworkPolicy decides whether a resolved address may be connected to.
type NetworkPolicy func(addr netip.Addr) error

// PublicOnly is the default network policy. It refuses every address that is
// not a routable public unicast address: loopback, RFC 1918 and unique-local
// private ranges, link-local (which is where cloud metadata services live),
// carrier-grade NAT, multicast, the unspecified address, documentation and
// benchmarking ranges, and the IPv6 prefixes that embed an IPv4 address and
// would otherwise smuggle a private one past the check.
func PublicOnly(addr netip.Addr) error {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsLoopback() || addr.IsPrivate() || addr.IsUnspecified() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsMulticast() || addr.IsInterfaceLocalMulticast() {
		return fmt.Errorf("%w: %s is not a public address", ErrBlockedAddress, addr)
	}
	for _, p := range blockedPrefixes {
		if p.Contains(addr) {
			return fmt.Errorf("%w: %s is in reserved range %s", ErrBlockedAddress, addr, p)
		}
	}
	return nil
}

var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8",       // "this network"
		"100.64.0.0/10",   // carrier-grade NAT
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // documentation
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // documentation
		"203.0.113.0/24",  // documentation
		"240.0.0.0/4",     // reserved, includes broadcast
		"64:ff9b::/96",    // NAT64: embeds an IPv4 address
		"64:ff9b:1::/48",  // local-use NAT64
		"2002::/16",       // 6to4: embeds an IPv4 address
		"2001::/32",       // Teredo: embeds an IPv4 address
		"2001:db8::/32",   // documentation
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// Fetcher downloads URLs into a blob store. It is safe for concurrent use.
type Fetcher struct {
	store          blob.Store
	allow          Allow
	network        NetworkPolicy
	headers        func(u *url.URL) http.Header
	requestHeaders func(context.Context, *url.URL) (http.Header, error)
	client         *http.Client
	timeout        time.Duration
	maxBytes       int64
	maxRedirects   int
	plainHTTP      bool
}

// Option configures a Fetcher.
type Option func(*Fetcher)

// WithHeaders sets the function that supplies request headers, and so
// credentials, for a URL. It is called for every hop with that hop's URL, and
// what it returns is sent to that URL and nowhere else. This is how a bearer
// token is scoped: return it for the host it belongs to and nil otherwise.
//
//	intake.WithHeaders(func(u *url.URL) http.Header {
//		if u.Hostname() != "files.slack.com" {
//			return nil
//		}
//		return http.Header{"Authorization": {"Bearer " + botToken}}
//	})
func WithHeaders(fn func(u *url.URL) http.Header) Option {
	return func(f *Fetcher) { f.headers = fn }
}

// WithRequestHeaders obtains rotating credentials for each validated request,
// including redirect hops. Failure stops the fetch before any HTTP request.
func WithRequestHeaders(fn func(context.Context, *url.URL) (http.Header, error)) Option {
	return func(f *Fetcher) { f.requestHeaders = fn }
}

// WithTimeout bounds a whole fetch. Non-positive values are ignored.
func WithTimeout(d time.Duration) Option {
	return func(f *Fetcher) {
		if d > 0 {
			f.timeout = d
		}
	}
}

// WithMaxBytes sets the size cap applied when a Fetch names none.
func WithMaxBytes(n int64) Option {
	return func(f *Fetcher) {
		if n > 0 {
			f.maxBytes = n
		}
	}
}

// WithMaxRedirects bounds the redirect chain. Zero refuses every redirect.
func WithMaxRedirects(n int) Option {
	return func(f *Fetcher) {
		if n >= 0 {
			f.maxRedirects = n
		}
	}
}

// WithNetworkPolicy replaces PublicOnly. It exists for tests, which serve
// from loopback, and for a deployment whose file host is genuinely on a
// private network. Replacing it removes the SSRF guard, so the replacement
// should admit specific addresses rather than everything.
func WithNetworkPolicy(p NetworkPolicy) Option {
	return func(f *Fetcher) {
		if p != nil {
			f.network = p
		}
	}
}

// WithPlainHTTP permits http:// URLs. HTTPS is required by default because a
// file fetched in the clear can be replaced in transit, and because a bearer
// token sent in the clear is a token disclosed.
func WithPlainHTTP() Option {
	return func(f *Fetcher) { f.plainHTTP = true }
}

// NewFetcher returns a Fetcher that stores into store and requests only what
// allow admits. allow is required and has no default: a nil policy refuses
// every URL, so forgetting to choose one fails closed.
func NewFetcher(store blob.Store, allow Allow, opts ...Option) *Fetcher {
	f := &Fetcher{
		store:        store,
		allow:        allow,
		network:      PublicOnly,
		timeout:      DefaultTimeout,
		maxBytes:     DefaultMaxBytes,
		maxRedirects: DefaultMaxRedirects,
	}
	for _, opt := range opts {
		opt(f)
	}
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		// Control runs once the address to connect to is known and before
		// the connection is made. It sees the resolved IP, never the name,
		// so it holds whatever DNS answered.
		Control: func(_, addr string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(addr)
			if err != nil {
				return fmt.Errorf("%w: unparseable dial address %q", ErrBlockedAddress, addr)
			}
			return f.network(ap.Addr())
		},
	}
	f.client = &http.Client{
		Transport: &http.Transport{
			// No proxy, deliberately. Through a proxy the dialed address is
			// the proxy's, and the check above would be inspecting the wrong
			// machine.
			Proxy:                 nil,
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
			MaxIdleConns:          8,
			IdleConnTimeout:       30 * time.Second,
		},
		// Redirects are followed by hand so that each hop is validated and
		// given its own credentials.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return f
}

// Fetch downloads rawURL into the tenant's blob store and returns the
// attachment that refers to it.
//
// The content's media type is sniffed, never taken from the response's
// Content-Type. A connector that was told to expect an image should compare
// the result: a surface that answers an unauthorized file request with a 200
// and an HTML sign-in page (Slack does) is detected by the attachment coming
// back as text/html.
func (f *Fetcher) Fetch(ctx context.Context, tenant, rawURL string, meta Meta) (attachment.Attachment, error) {
	if f.store == nil {
		return attachment.Attachment{}, errors.New("intake: no blob store")
	}
	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()

	u, err := url.Parse(rawURL)
	if err != nil {
		return attachment.Attachment{}, fmt.Errorf("%w: unparseable url", ErrNotAllowed)
	}
	if meta.MaxBytes <= 0 {
		meta.MaxBytes = f.maxBytes
	}

	for hop := 0; ; hop++ {
		if err := f.permit(u); err != nil {
			return attachment.Attachment{}, err
		}
		resp, err := f.get(ctx, u)
		if err != nil {
			return attachment.Attachment{}, err
		}
		if next, redirect, err := redirectTarget(u, resp); err != nil {
			_ = resp.Body.Close()
			return attachment.Attachment{}, err
		} else if redirect {
			_ = resp.Body.Close()
			if hop >= f.maxRedirects {
				return attachment.Attachment{}, fmt.Errorf("%w: more than %d", ErrTooManyRedirects, f.maxRedirects)
			}
			u = next
			continue
		}

		att, err := f.store200(ctx, tenant, u, resp, meta)
		_ = resp.Body.Close()
		return att, err
	}
}

// permit applies the checks that need no network: the URL's shape, then the
// host policy.
func (f *Fetcher) permit(u *url.URL) error {
	schemeOK := u.Scheme == "https" || (f.plainHTTP && u.Scheme == "http")
	switch {
	case !schemeOK:
		return fmt.Errorf("%w: scheme %q, only https is fetched", ErrNotAllowed, u.Scheme)
	case u.Hostname() == "":
		return fmt.Errorf("%w: no host", ErrNotAllowed)
	case u.User != nil:
		return fmt.Errorf("%w: credentials embedded in the url", ErrNotAllowed)
	case f.allow == nil:
		return fmt.Errorf("%w: no host policy configured", ErrNotAllowed)
	}
	// A literal IP is checked here as well as at dial time, so the refusal
	// names the reason instead of surfacing as a failed connection.
	if addr, err := netip.ParseAddr(u.Hostname()); err == nil {
		if err := f.network(addr); err != nil {
			return err
		}
	}
	if err := f.allow(u); err != nil {
		if !errors.Is(err, ErrNotAllowed) {
			err = fmt.Errorf("%w: %w", ErrNotAllowed, err)
		}
		return err
	}
	return nil
}

func (f *Fetcher) get(ctx context.Context, u *url.URL) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotAllowed, err)
	}
	req.Header.Set("User-Agent", "fiber-attachment-intake")
	req.Header.Set("Accept", "*/*")
	if f.requestHeaders != nil {
		headers, err := f.requestHeaders(ctx, u)
		if err != nil {
			return nil, errors.New("intake: request credentials unavailable")
		}
		for k, vs := range headers {
			req.Header[k] = append([]string(nil), vs...)
		}
	} else if f.headers != nil {
		for k, vs := range f.headers(u) {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
	}
	resp, err := f.client.Do(req)
	if err != nil {
		// The URL may carry a signed query; keep it out of the error.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("intake: fetch from %s: %w", u.Hostname(), err)
	}
	return resp, nil
}

// redirectTarget reports where a redirect response points.
func redirectTarget(from *url.URL, resp *http.Response) (*url.URL, bool, error) {
	switch resp.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
	default:
		return nil, false, nil
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		return nil, false, fmt.Errorf("%w: redirect from %s with no location", ErrStatus, from.Hostname())
	}
	next, err := from.Parse(loc)
	if err != nil {
		return nil, false, fmt.Errorf("%w: unparseable redirect from %s", ErrNotAllowed, from.Hostname())
	}
	return next, true, nil
}

func (f *Fetcher) store200(ctx context.Context, tenant string, u *url.URL, resp *http.Response, meta Meta) (attachment.Attachment, error) {
	if resp.StatusCode != http.StatusOK {
		return attachment.Attachment{}, &StatusError{Code: resp.StatusCode, Host: u.Hostname()}
	}
	// A declared length over the cap is refused without reading a byte. An
	// absent or dishonest length is still held to the cap by the store.
	if resp.ContentLength > meta.MaxBytes {
		return attachment.Attachment{}, fmt.Errorf("intake: fetch from %s: %w: declared %d bytes, limit %d",
			u.Hostname(), blob.ErrTooLarge, resp.ContentLength, meta.MaxBytes)
	}
	if meta.Name == "" {
		meta.Name = responseName(u, resp)
	}
	return Store(ctx, f.store, tenant, resp.Body, meta)
}

// responseName picks a display name when the connector supplied none: the
// response's suggested file name, or the last segment of the URL path. Either
// is cleaned by Store before it is used.
func responseName(u *url.URL, resp *http.Response) string {
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil && params["filename"] != "" {
		return params["filename"]
	}
	if base := path.Base(u.Path); base != "/" && base != "." {
		return base
	}
	return ""
}
