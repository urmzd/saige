package netguard

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"syscall"
	"time"
)

// DefaultMaxRedirects is how many redirects a Client follows unless told
// otherwise, the same bound net/http applies.
const DefaultMaxRedirects = 10

// Control returns a net.Dialer Control hook that refuses any connection p
// refuses. It runs once the address to connect to is resolved and before
// the connection is made, so it sees the IP actually dialed. A dial address
// that is not an IP and port is refused. A nil p is PublicOnly.
func Control(p Policy) func(network, address string, c syscall.RawConn) error {
	if p == nil {
		p = PublicOnly
	}
	return func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil {
			return &BlockedError{Address: strconv.Quote(address), Reason: "not an IP address and port"}
		}
		return p(ap.Addr())
	}
}

// Dialer returns a dialer whose every connection is checked against p (see
// Control), with the timeouts of net/http's default transport. A caller may
// adjust the timeouts; replacing Control removes the guard.
func Dialer(p Policy) *net.Dialer {
	return &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   Control(p),
	}
}

// Transport returns an HTTP transport that dials through Dialer(p) and uses
// no proxy, with the remaining settings of net/http's default transport. It
// is a new value each call, so a caller may tune its timeouts and pool;
// setting Proxy or DialContext removes the guard.
func Transport(p Policy) *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           Dialer(p).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// ClientOption configures Client.
type ClientOption func(*clientConfig)

type clientConfig struct {
	maxRedirects int
}

// WithMaxRedirects bounds the redirect chain a Client follows. Zero follows
// none: the redirect response itself is returned, for a caller that follows
// redirects by hand and checks each hop against a host policy of its own.
// A negative n is ignored.
func WithMaxRedirects(n int) ClientOption {
	return func(c *clientConfig) {
		if n >= 0 {
			c.maxRedirects = n
		}
	}
}

// Client returns an HTTP client for URLs that come from a model, a user or
// configuration rather than code. Every connection, including each redirect
// hop, is checked against p after DNS (see Control). Redirects are followed
// up to DefaultMaxRedirects (see WithMaxRedirects), and only to http and
// https URLs. The client sets no overall timeout; bound a request with its
// context or set Timeout on the result. A nil p is PublicOnly.
func Client(p Policy, opts ...ClientOption) *http.Client {
	cfg := clientConfig{maxRedirects: DefaultMaxRedirects}
	for _, o := range opts {
		o(&cfg)
	}
	return &http.Client{
		Transport: Transport(p),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if cfg.maxRedirects == 0 {
				return http.ErrUseLastResponse
			}
			if len(via) >= cfg.maxRedirects {
				return fmt.Errorf("netguard: stopped after %d redirects", cfg.maxRedirects)
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("netguard: redirect to unsupported scheme %q", req.URL.Scheme)
			}
			return nil
		},
	}
}
