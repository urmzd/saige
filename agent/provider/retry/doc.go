// Package retry decorates a Provider with configurable retry and backoff,
// re-issuing failed ChatStream calls before surfacing an error. Compose with
// package fallback for multi-provider resilience.
package retry
