// Package fallback composes multiple Providers into one that tries each in
// order until a Stream call succeeds. Compose with package retry for
// per-provider retry before falling through.
//
// By default a terminal error (cancellation, an invalid request, an exhausted
// budget) stops the chain instead of paying each member to reject the same
// request; see DefaultFallbackOn.
//
// A configuration names the failures that fall through by condition (OnAny,
// OnUnavailable, OnRateLimit, OnTransient, OnAuth, OnContextLength,
// OnContentFilter), and Rule turns the names into a FallbackOn predicate.
package fallback
