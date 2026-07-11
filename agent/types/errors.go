package types

import (
	"errors"
	"fmt"
	"net/http"
)

// ── Sentinel errors ──────────────────────────────────────────────────

var (
	ErrToolNotFound         = errors.New("tool not found")
	ErrMaxIterations        = errors.New("max iterations reached")
	ErrStreamCanceled       = errors.New("stream canceled")
	ErrProviderFailed       = errors.New("provider failed")
	ErrUnsupportedMediaType = errors.New("unsupported media type")
	ErrResolverNotFound     = errors.New("no resolver for URI scheme")
)

// ── Error classification ─────────────────────────────────────────────

// ErrorKind classifies errors as transient (retry-worthy) or permanent.
type ErrorKind int

const (
	ErrorKindTransient ErrorKind = iota // retry-worthy (429, 5xx, timeout, connection refused)
	ErrorKindPermanent                  // do not retry (4xx auth, bad request, etc.)
)

// IsTransient returns true if err is a transient (retry-worthy) error.
func IsTransient(err error) bool {
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe.Kind == ErrorKindTransient
	}
	// Unwrap FallbackError: transient if the last error was transient.
	var fe *FallbackError
	if errors.As(err, &fe) && len(fe.Errors) > 0 {
		return IsTransient(fe.Errors[len(fe.Errors)-1])
	}
	return false
}

// ClassifyHTTPStatus maps an HTTP status code to an ErrorKind.
func ClassifyHTTPStatus(code int) ErrorKind {
	switch {
	case code == http.StatusTooManyRequests: // 429
		return ErrorKindTransient
	case code == http.StatusRequestTimeout: // 408
		return ErrorKindTransient
	case code >= 500 && code < 600: // 5xx
		return ErrorKindTransient
	default:
		return ErrorKindPermanent
	}
}

// ── Structured error types ───────────────────────────────────────────

// ProviderError is a rich error from a provider call.
// errors.Is(err, ErrProviderFailed) returns true.
type ProviderError struct {
	Provider string    // provider name (e.g. "ollama", "openai")
	Model    string    // model that was called
	Kind     ErrorKind // transient or permanent
	Code     int       // HTTP status code, 0 if not applicable
	Err      error     // underlying error
}

func (e *ProviderError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("provider %s (model %s, status %d): %v", e.Provider, e.Model, e.Code, e.Err)
	}
	return fmt.Sprintf("provider %s (model %s): %v", e.Provider, e.Model, e.Err)
}

func (e *ProviderError) Unwrap() error { return e.Err }

func (e *ProviderError) Is(target error) bool { return target == ErrProviderFailed }

// FallbackError is returned when all providers in a FallbackProvider fail.
type FallbackError struct {
	Errors []error // one per provider attempted, in order
}

func (e *FallbackError) Error() string {
	return fmt.Sprintf("all %d providers failed: %v", len(e.Errors), errors.Join(e.Errors...))
}

// Unwrap returns the list of errors for Go 1.20+ multi-unwrap.
func (e *FallbackError) Unwrap() []error { return e.Errors }

func (e *FallbackError) Is(target error) bool { return target == ErrProviderFailed }

// RetryError is returned when all retry attempts are exhausted.
type RetryError struct {
	Attempts int
	Last     error // the final error
}

func (e *RetryError) Error() string {
	return fmt.Sprintf("failed after %d attempts: %v", e.Attempts, e.Last)
}

func (e *RetryError) Unwrap() error { return e.Last }
