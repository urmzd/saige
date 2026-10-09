package mcp

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
var ErrBlockedAddress = errors.New("mcp: destination address is blocked")

// metadataPrefixes are cloud instance-metadata ranges. They are always
// blocked: a server URL that reaches them can read instance credentials.
var metadataPrefixes = []netip.Prefix{
	netip.MustParsePrefix("169.254.0.0/16"),     // link-local, incl. AWS/GCP/Azure metadata
	netip.MustParsePrefix("fe80::/10"),          // IPv6 link-local
	netip.MustParsePrefix("fd00:ec2::254/128"),  // AWS IPv6 metadata
	netip.MustParsePrefix("100.100.100.200/32"), // Alibaba Cloud metadata
}

// SafeHTTPClient returns an HTTP client for server URLs that come from
// configuration rather than code. It checks the resolved IP address at
// connect time, after DNS, so a hostname that re-resolves to a forbidden
// address between check and use is still refused.
//
// Cloud metadata and link-local addresses are always refused. With
// blockPrivate, loopback, private (RFC 1918 and IPv6 unique local),
// carrier-grade NAT, unspecified and multicast addresses are refused too. The
// client ignores proxy environment variables, since a proxy would make the
// dialed address meaningless.
func SafeHTTPClient(blockPrivate bool) *http.Client {
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			return checkAddress(address, blockPrivate)
		},
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, addr)
	}
	return &http.Client{Transport: transport}
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func checkAddress(address string, blockPrivate bool) error {
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
