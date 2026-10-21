package mcp

import (
	"errors"
	"net/http"

	"github.com/urmzd/saige/netguard"
)

// ErrBlockedAddress is returned when SafeHTTPClient refuses to dial an
// address. Such an error matches netguard.ErrBlocked too.
var ErrBlockedAddress = errors.New("mcp: destination address is blocked")

// SafeHTTPClient returns an HTTP client for server URLs that come from
// configuration rather than code: netguard.Client with netguard.NoMetadata,
// or with netguard.PublicOnly when blockPrivate is set, its refusals tagged
// with ErrBlockedAddress. The resolved IP address is checked at connect
// time, after DNS, so a hostname that re-resolves to a forbidden address
// between check and use is still refused.
//
// Cloud metadata and link-local addresses are always refused. With
// blockPrivate, every address that is not public unicast is refused too:
// loopback, private (RFC 1918 and IPv6 unique local), carrier-grade NAT,
// unspecified, multicast and reserved ranges. Proxy environment variables
// are ignored, since a proxy would make the dialed address meaningless.
func SafeHTTPClient(blockPrivate bool) *http.Client {
	return netguard.Client(addressPolicy(blockPrivate))
}

// addressPolicy is the netguard policy SafeHTTPClient applies.
func addressPolicy(blockPrivate bool) netguard.Policy {
	if blockPrivate {
		return netguard.Tag(netguard.PublicOnly, ErrBlockedAddress)
	}
	return netguard.Tag(netguard.NoMetadata, ErrBlockedAddress)
}
