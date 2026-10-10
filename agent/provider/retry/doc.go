// Package retry decorates a Provider with configurable retry and backoff,
// re-issuing failed Stream calls before surfacing an error. Compose with
// package fallback for multi-provider resilience.
//
// This decorator is meant to be the only retry layer. The anthropic and openai
// adapters disable their SDKs' built-in retries by default (WithMaxRetries(0)),
// so every attempt is counted in RetryError, visible to metrics and budgets,
// and paced by one backoff policy. Waits use full jitter and honor a
// provider's Retry-After, carried on types.ProviderError.RetryAfter.
package retry
