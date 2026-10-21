// Package fetch provides a fetch tool that retrieves a URL and returns its
// text, through an HTTP client that refuses private, local, and cloud
// metadata addresses.
package fetch

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"

	"github.com/urmzd/saige/netguard"
)

// ErrBlockedAddress is returned when SafeHTTPClient refuses to dial an
// address. Such an error matches netguard.ErrBlocked too.
var ErrBlockedAddress = errors.New("fetch: destination address is blocked")

// maxRedirects bounds the redirect chain SafeHTTPClient follows.
const maxRedirects = 5

// SafeHTTPClient returns an HTTP client for URLs chosen by a model or a
// user: netguard.Client with netguard.PublicOnly, or netguard.NoMetadata
// when blockPrivate is false, its refusals tagged with ErrBlockedAddress.
// The resolved IP address is checked at connect time, after DNS, so a
// hostname that resolves to a forbidden address, including on a redirect or
// a re-resolution, is refused.
//
// Cloud metadata and link-local addresses are always refused. With
// blockPrivate, every address that is not public unicast is refused too:
// loopback, private (RFC 1918 and IPv6 unique local), carrier-grade NAT,
// unspecified, multicast, and reserved ranges. Proxy environment variables
// are ignored, since a proxy would make the dialed address meaningless. At
// most five redirects are followed, and only to http and https.
func SafeHTTPClient(blockPrivate bool) *http.Client {
	return netguard.Client(addressPolicy(blockPrivate), netguard.WithMaxRedirects(maxRedirects))
}

// CheckAddress reports whether host:port (or a bare IP) may be dialed under
// SafeHTTPClient's policy. The address must be a literal IP, as it is once
// DNS has resolved it.
func CheckAddress(address string, blockPrivate bool) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("%w: %q is not an IP address", ErrBlockedAddress, host)
	}
	return addressPolicy(blockPrivate)(ip)
}

// addressPolicy is the netguard policy SafeHTTPClient applies.
func addressPolicy(blockPrivate bool) netguard.Policy {
	if blockPrivate {
		return netguard.Tag(netguard.PublicOnly, ErrBlockedAddress)
	}
	return netguard.Tag(netguard.NoMetadata, ErrBlockedAddress)
}
