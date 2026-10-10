// Package fallback composes multiple Providers into one that tries each in
// order until a Stream call succeeds. Compose with package retry for
// per-provider retry before falling through.
//
// By default a terminal error (cancellation, an invalid request, an exhausted
// budget) stops the chain instead of paying each member to reject the same
// request; see DefaultFallbackOn.
package fallback
