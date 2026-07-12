// Package fallback composes multiple Providers into one that tries each in
// order until a ChatStream call succeeds. Compose with package retry for
// per-provider retry before falling through.
package fallback
