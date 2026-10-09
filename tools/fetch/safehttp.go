// Package fetch provides a fetch tool that retrieves a URL and returns its
// text, through an HTTP client that refuses private, local, and cloud
// metadata addresses.
package fetch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// ErrBlockedAddress is returned when SafeHTTPClient refuses to dial an
// address.
var ErrBlockedAddress = errors.New("fetch: destination address is blocked")

// maxRedirects bounds the redirect chain SafeHTTPClient follows.
const maxRedirects = 5

// metadataPrefixes are always refused: an address in them can expose cloud
// instance credentials.
var metadataPrefixes = []netip.Prefix{
	netip.MustParsePrefix("169.254.0.0/16"),     // link-local, including AWS, GCP, and Azure metadata
	netip.MustParsePrefix("fe80::/10"),          // IPv6 link-local
	netip.MustParsePrefix("fd00:ec2::254/128"),  // AWS IPv6 metadata
	netip.MustParsePrefix("100.100.100.200/32"), // Alibaba Cloud metadata
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// SafeHTTPClient returns an HTTP client for URLs chosen by a model or a
// user. The resolved IP address is checked at connect time, after DNS, so a
// hostname that resolves to a forbidden address, including on a redirect or
// a re-resolution, is refused.
//
// Cloud metadata and link-local addresses are always refused. With
// blockPrivate, loopback, private (RFC 1918 and IPv6 unique local),
// carrier-grade NAT, unspecified, and multicast addresses are refused too.
// Proxy environment variables are ignored, since a proxy would make the
// dialed address meaningless. At most five redirects are followed.
func SafeHTTPClient(blockPrivate bool) *http.Client {
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			return CheckAddress(address, blockPrivate)
		},
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, addr)
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("fetch: stopped after %d redirects", maxRedirects)
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("fetch: redirect to unsupported scheme %q", req.URL.Scheme)
			}
			return nil
		},
	}
}

// CheckAddress reports whether host:port (or a bare IP) may be dialed. The
// address must be a literal IP, as it is once DNS has resolved it.
func CheckAddress(address string, blockPrivate bool) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("%w: %q is not an IP address", ErrBlockedAddress, host)
	}
	ip = ip.Unmap()
	for _, p := range metadataPrefixes {
		if p.Contains(ip) {
			return fmt.Errorf("%w: %s is a metadata or link-local address", ErrBlockedAddress, ip)
		}
	}
	if blockPrivate && (ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsMulticast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || cgnat.Contains(ip)) {
		return fmt.Errorf("%w: %s is a private or local address", ErrBlockedAddress, ip)
	}
	return nil
}
