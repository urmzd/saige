// Package netguard is the SSRF guard: it decides which addresses a client
// may connect to, and builds dialers, transports and HTTP clients that hold
// every connection to that decision.
//
// The check runs in the dialer's Control hook, once DNS has answered and
// before the connection is made, so it sees the IP address actually dialed
// and never a name. A host name that resolves, or re-resolves between a
// check and its use, to a forbidden address is refused, and so is every
// redirect hop, since each hop dials again. Proxy environment variables are
// ignored: through a proxy the dialed address is the proxy's, and the check
// would be inspecting the wrong machine.
//
// A Policy is the decision. PublicOnly admits routable public unicast
// addresses alone and is the right default for URLs chosen by a model or a
// user. NoMetadata refuses only link-local and cloud metadata addresses, for
// URLs from configuration that may name a private host. Except admits a few
// listed ranges ahead of a policy, which is how a test reaches an httptest
// server on loopback. Tag gives a policy's refusals a package's own sentinel
// as well as ErrBlocked.
package netguard

import (
	"errors"
	"net/netip"
)

// ErrBlocked matches every refusal a Policy in this package makes, and every
// connection a Dialer, Transport or Client refuses because of one.
var ErrBlocked = errors.New("netguard: destination address is blocked")

// BlockedError is a refusal: the address and why it was refused. It matches
// ErrBlocked.
type BlockedError struct {
	// Address is the address refused: an IP, or the quoted dial address
	// when that was not an IP.
	Address string
	// Reason completes "Address is ...", as in "not a public address".
	Reason string
}

func (e *BlockedError) Error() string { return ErrBlocked.Error() + ": " + e.detail() }

// Is makes a BlockedError match ErrBlocked.
func (e *BlockedError) Is(target error) bool { return target == ErrBlocked }

func (e *BlockedError) detail() string { return e.Address + " is " + e.Reason }

// Policy decides whether a resolved address may be connected to: nil admits
// it, and an error refuses it. A policy in this package refuses with a
// *BlockedError; one written elsewhere should wrap ErrBlocked, so callers can
// match a refusal with errors.Is.
type Policy func(addr netip.Addr) error

// PublicOnly admits routable public unicast addresses and refuses everything
// else: loopback, RFC 1918 and unique-local private ranges, link-local
// (where cloud metadata services live), carrier-grade NAT (which holds
// Alibaba Cloud's metadata address), multicast, unspecified and "this
// network" addresses, documentation, benchmarking and reserved ranges,
// deprecated IPv6 site-local and IPv4-compatible addresses, and the IPv6
// prefixes that embed an IPv4 address (NAT64, 6to4, Teredo) and would
// otherwise carry a private one past the check. An IPv4-mapped IPv6 address
// is judged as the IPv4 address it maps. The zero Addr is refused.
func PublicOnly(addr netip.Addr) error {
	addr = addr.Unmap()
	if !addr.IsValid() {
		return &BlockedError{Address: "the zero address", Reason: "not a public address"}
	}
	if addr.IsLoopback() || addr.IsPrivate() || addr.IsUnspecified() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsMulticast() || addr.IsInterfaceLocalMulticast() {
		return &BlockedError{Address: addr.String(), Reason: "not a public address"}
	}
	for _, p := range reservedPrefixes {
		if p.Contains(addr) {
			return &BlockedError{Address: addr.String(), Reason: "in reserved range " + p.String()}
		}
	}
	return nil
}

// NoMetadata refuses link-local addresses and the cloud instance-metadata
// addresses, which can hand out instance credentials, and admits everything
// else, loopback and private networks included. Use it where an address
// comes from configuration and may name a host on the local network; use
// PublicOnly where it comes from a model or a user. The zero Addr is refused.
func NoMetadata(addr netip.Addr) error {
	addr = addr.Unmap()
	if !addr.IsValid() {
		return &BlockedError{Address: "the zero address", Reason: "not an address"}
	}
	for _, p := range metadataPrefixes {
		if p.Contains(addr) {
			return &BlockedError{Address: addr.String(), Reason: "a metadata or link-local address"}
		}
	}
	return nil
}

// Except returns a policy that admits addresses in the listed prefixes and
// hands every other address to p. It exists for tests, which serve from
// loopback, and for a deployment whose host genuinely sits on a private
// network: name the specific addresses, never a whole private range.
// Addresses are compared unmapped, so 127.0.0.1/32 also admits
// ::ffff:127.0.0.1. A nil p is PublicOnly.
//
//	netguard.Except(netguard.PublicOnly, netip.MustParsePrefix("127.0.0.1/32"))
func Except(p Policy, admit ...netip.Prefix) Policy {
	if p == nil {
		p = PublicOnly
	}
	admit = append([]netip.Prefix(nil), admit...)
	return func(addr netip.Addr) error {
		a := addr.Unmap()
		for _, pre := range admit {
			if pre.Contains(a) {
				return nil
			}
		}
		return p(addr)
	}
}

// Tag returns a policy that refuses what p refuses, with errors that match
// sentinel as well as ErrBlocked and that read as sentinel followed by the
// reason. A package keeps its own sentinel, and its wire code, this way:
//
//	var ErrBlockedAddress = errors.New("mcp: destination address is blocked")
//	policy := netguard.Tag(netguard.NoMetadata, ErrBlockedAddress)
//
// A nil p is PublicOnly; a nil sentinel returns p unchanged.
func Tag(p Policy, sentinel error) Policy {
	if p == nil {
		p = PublicOnly
	}
	if sentinel == nil {
		return p
	}
	return func(addr netip.Addr) error {
		err := p(addr)
		if err == nil || errors.Is(err, sentinel) {
			return err
		}
		return &taggedError{sentinel: sentinel, err: err}
	}
}

// taggedError is a refusal under a second sentinel. It matches both the
// sentinel and the refusal it wraps.
type taggedError struct {
	sentinel error
	err      error
}

func (e *taggedError) Error() string {
	var be *BlockedError
	if errors.As(e.err, &be) {
		return e.sentinel.Error() + ": " + be.detail()
	}
	return e.sentinel.Error() + ": " + e.err.Error()
}

func (e *taggedError) Unwrap() []error { return []error{e.sentinel, e.err} }

// metadataPrefixes are refused by every policy here: an address in them can
// expose cloud instance credentials.
var metadataPrefixes = []netip.Prefix{
	netip.MustParsePrefix("169.254.0.0/16"),     // link-local, including AWS, GCP and Azure metadata
	netip.MustParsePrefix("fe80::/10"),          // IPv6 link-local
	netip.MustParsePrefix("fd00:ec2::254/128"),  // AWS IPv6 metadata
	netip.MustParsePrefix("100.100.100.200/32"), // Alibaba Cloud metadata
}

// reservedPrefixes are the ranges PublicOnly refuses beyond what the netip
// predicates cover.
var reservedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8",       // "this network"
		"100.64.0.0/10",   // carrier-grade NAT, including Alibaba Cloud metadata
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // documentation
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // documentation
		"203.0.113.0/24",  // documentation
		"240.0.0.0/4",     // reserved, includes broadcast
		"::/96",           // IPv4-compatible (deprecated): embeds an IPv4 address
		"64:ff9b::/96",    // NAT64: embeds an IPv4 address
		"64:ff9b:1::/48",  // local-use NAT64
		"2002::/16",       // 6to4: embeds an IPv4 address
		"2001::/32",       // Teredo: embeds an IPv4 address
		"2001:db8::/32",   // documentation
		"fec0::/10",       // site-local (deprecated)
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()
