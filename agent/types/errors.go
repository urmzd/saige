package types

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ── Sentinel errors ──────────────────────────────────────────────────

var (
	// ErrToolNotFound reports a call to a tool the registry does not hold.
	ErrToolNotFound = errors.New("tool not found")
	// ErrMaxIterations reports a run that reached its iteration limit.
	ErrMaxIterations = errors.New("max iterations reached")
	// ErrStreamCanceled reports a stream its consumer canceled.
	ErrStreamCanceled = errors.New("stream canceled")
	// ErrProviderFailed matches every *ProviderError and *FallbackError.
	ErrProviderFailed = errors.New("provider failed")
	// ErrUnsupportedMediaType reports media no resolver or extractor takes.
	ErrUnsupportedMediaType = errors.New("unsupported media type")
	// ErrResolverNotFound reports a URI scheme no resolver serves.
	ErrResolverNotFound = errors.New("no resolver for URI scheme")
	// ErrInvalidConfig reports a constructor given a configuration it
	// cannot build from: a missing required field, a value out of range,
	// or fields that contradict each other. Every New in this module
	// returns an error wrapping it instead of panicking.
	ErrInvalidConfig = errors.New("invalid configuration")
)

// Kind sentinels. A ProviderError matches the sentinel for its Kind, so
// callers can write errors.Is(err, ErrContextLength) without inspecting the
// struct or matching provider message text.
var (
	ErrContextLength     = errors.New("context length exceeded")
	ErrContentFiltered   = errors.New("content filtered")
	ErrAuth              = errors.New("authentication failed")
	ErrRateLimited       = errors.New("rate limited")
	ErrUnavailable       = errors.New("provider unavailable")
	ErrInvalidRequest    = errors.New("invalid request")
	ErrResponseTruncated = errors.New("response truncated")
)

// ── Error classification ─────────────────────────────────────────────

// ErrorKind classifies an error by cause. The two original kinds remain the
// coarse classes; the finer kinds let callers react to a specific failure
// (shrink the context, back off, re-authenticate) without parsing messages.
// Use Transient to decide whether a retry can help.
type ErrorKind int

const (
	ErrorKindTransient      ErrorKind = iota // retry-worthy (408, other 5xx, connection reset, timeout)
	ErrorKindPermanent                       // do not retry; no finer cause known
	ErrorKindContextLength                   // the prompt exceeds the model's context window
	ErrorKindContentFilter                   // the provider's safety system blocked input or output
	ErrorKindAuth                            // missing, invalid, or insufficient credentials (401, 403)
	ErrorKindRateLimit                       // quota or rate limit hit (429); honor RetryAfter
	ErrorKindUnavailable                     // overloaded or down (500, 502, 503, 504, 529)
	ErrorKindInvalidRequest                  // the request is malformed or unsupported (400, 404, 413, 422)
	ErrorKindTruncated                       // the response hit the output token limit
)

var errorKindNames = [...]string{
	ErrorKindTransient:      "transient",
	ErrorKindPermanent:      "permanent",
	ErrorKindContextLength:  "context_length",
	ErrorKindContentFilter:  finishContentFilter,
	ErrorKindAuth:           "auth",
	ErrorKindRateLimit:      "rate_limit",
	ErrorKindUnavailable:    "unavailable",
	ErrorKindInvalidRequest: "invalid_request",
	ErrorKindTruncated:      "truncated",
}

// String returns the stable snake_case name used on the wire.
func (k ErrorKind) String() string {
	if k >= 0 && int(k) < len(errorKindNames) {
		return errorKindNames[k]
	}
	return "permanent"
}

// ParseErrorKind is the inverse of String. Unknown names map to
// ErrorKindPermanent, so a newer peer's kind never becomes retry-worthy here.
func ParseErrorKind(s string) ErrorKind {
	for k, name := range errorKindNames {
		if name == s {
			return ErrorKind(k)
		}
	}
	return ErrorKindPermanent
}

// Transient reports whether a retry of the same request can succeed.
func (k ErrorKind) Transient() bool {
	switch k {
	case ErrorKindTransient, ErrorKindRateLimit, ErrorKindUnavailable:
		return true
	default:
		return false
	}
}

func (k ErrorKind) sentinel() error {
	switch k {
	case ErrorKindContextLength:
		return ErrContextLength
	case ErrorKindContentFilter:
		return ErrContentFiltered
	case ErrorKindAuth:
		return ErrAuth
	case ErrorKindRateLimit:
		return ErrRateLimited
	case ErrorKindUnavailable:
		return ErrUnavailable
	case ErrorKindInvalidRequest:
		return ErrInvalidRequest
	case ErrorKindTruncated:
		return ErrResponseTruncated
	default:
		return nil
	}
}

// KindReporter is implemented by errors that carry an ErrorKind:
// *ProviderError, *RemoteError, *ResponseTruncatedError and *FallbackError.
// KindOf and IsTransient find the first one in an error's chain with
// errors.As, so an error type outside this package classifies itself by
// implementing it.
type KindReporter interface {
	error
	ErrorKind() ErrorKind
}

// IsTransient reports whether err is a transient (retry-worthy) error: its
// KindOf is transient.
func IsTransient(err error) bool {
	return KindOf(err).Transient()
}

// KindOf returns the ErrorKind of the first KindReporter in err's chain.
// Errors without one are permanent. A FallbackError reports the kind of its
// last attempt (see FallbackError).
func KindOf(err error) ErrorKind {
	var kr KindReporter
	if errors.As(err, &kr) {
		return kr.ErrorKind()
	}
	return ErrorKindPermanent
}

// RetryAfter returns the server-requested delay carried by err, or 0: the
// delay of the first *ProviderError in its chain. A FallbackError's chain
// is its last attempt, and a decoded error keeps the provider error it
// carried, so the delay survives both.
func RetryAfter(err error) time.Duration {
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe.RetryAfter
	}
	return 0
}

// IsContextLength reports whether err means the prompt is too long for the model.
func IsContextLength(err error) bool { return errors.Is(err, ErrContextLength) }

// IsContentFilter reports whether a provider safety system blocked the request or response.
func IsContentFilter(err error) bool { return errors.Is(err, ErrContentFiltered) }

// IsAuth reports whether err is an authentication or authorization failure.
func IsAuth(err error) bool { return errors.Is(err, ErrAuth) }

// IsRateLimit reports whether err is a rate-limit or quota failure.
func IsRateLimit(err error) bool { return errors.Is(err, ErrRateLimited) }

// IsUnavailable reports whether the provider was overloaded or down.
func IsUnavailable(err error) bool { return errors.Is(err, ErrUnavailable) }

// IsTruncated reports whether the response stopped at the output token limit.
func IsTruncated(err error) bool { return errors.Is(err, ErrResponseTruncated) }

// ClassifyHTTPStatus maps an HTTP status code to the coarse transient or
// permanent class. Use ClassifyHTTPStatusKind or ClassifyHTTPError for the
// finer kinds; both agree with this function on Transient().
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

// statusOverloaded is the non-standard status Anthropic uses for overload.
const statusOverloaded = 529

// ClassifyHTTPStatusKind maps an HTTP status code to the finest ErrorKind the
// status alone supports.
func ClassifyHTTPStatusKind(code int) ErrorKind {
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrorKindAuth
	case http.StatusTooManyRequests:
		return ErrorKindRateLimit
	case http.StatusRequestTimeout:
		return ErrorKindTransient
	case http.StatusBadRequest, http.StatusNotFound, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return ErrorKindInvalidRequest
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable,
		http.StatusGatewayTimeout, statusOverloaded:
		return ErrorKindUnavailable
	}
	if code >= 500 && code < 600 {
		return ErrorKindTransient
	}
	return ErrorKindPermanent
}

// ClassifyHTTPError combines the status code, the response headers, and the
// provider's error message into a kind and a retry delay. The message refines
// the status: a 400 that says the prompt is too long is ErrorKindContextLength,
// not ErrorKindInvalidRequest. header may be nil.
func ClassifyHTTPError(code int, header http.Header, message string) (ErrorKind, time.Duration) {
	kind := ClassifyHTTPStatusKind(code)
	if k, ok := ClassifyErrorMessage(message); ok && !kind.Transient() {
		kind = k
	}
	var delay time.Duration
	if kind.Transient() {
		delay = RetryAfterFromHeader(header, time.Now())
	}
	return kind, delay
}

var contextLengthPhrases = []string{
	"context_length_exceeded",
	"context length",
	"context window",
	"maximum context",
	"prompt is too long",
	"input is too long",
	"too many tokens",
	"token limit",
	"reduce the length",
}

var contentFilterPhrases = []string{
	"content_filter",
	"content filter",
	"content_policy",
	"content policy",
	"content management policy",
	"safety system",
	"blocked due to safety",
}

// ClassifyErrorMessage recognizes provider error text that names a cause the
// status code does not: context-length overflow and content filtering. It
// returns false when the message matches neither.
func ClassifyErrorMessage(message string) (ErrorKind, bool) {
	m := strings.ToLower(message)
	for _, p := range contextLengthPhrases {
		if strings.Contains(m, p) {
			return ErrorKindContextLength, true
		}
	}
	for _, p := range contentFilterPhrases {
		if strings.Contains(m, p) {
			return ErrorKindContentFilter, true
		}
	}
	return 0, false
}

// RetryAfterFromHeader reads the server-requested delay from Retry-After-Ms
// (milliseconds) or Retry-After (delta-seconds or an HTTP date). It returns 0
// when neither is present or parseable.
func RetryAfterFromHeader(h http.Header, now time.Time) time.Duration {
	if h == nil {
		return 0
	}
	if v := strings.TrimSpace(h.Get("Retry-After-Ms")); v != "" {
		if ms, err := strconv.ParseFloat(v, 64); err == nil && ms > 0 {
			return time.Duration(ms * float64(time.Millisecond))
		}
	}
	return ParseRetryAfter(h.Get("Retry-After"), now)
}

// ParseRetryAfter parses a Retry-After value: delta-seconds (fractions
// allowed) or an HTTP date relative to now. Invalid, negative, and past
// values return 0.
func ParseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

var transportPhrases = []string{
	"connection reset",
	"connection refused",
	"broken pipe",
	"unexpected eof",
	"server sent goaway",
	"tls handshake timeout",
	"i/o timeout",
}

// ClassifyTransportError reports whether err is a network failure that a
// retry can fix: a reset or refused connection, an EOF mid-response, a dial
// failure, or a timeout. Callers must apply it only before the first token
// of a response has been delivered; after that, a retry would duplicate
// output the consumer already saw. Caller cancellation (context.Canceled) is
// never transient. It returns false for nil and for unrecognized errors.
func ClassifyTransportError(err error) (ErrorKind, bool) {
	if err == nil || errors.Is(err, context.Canceled) {
		return 0, false
	}
	switch {
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF),
		errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.ECONNREFUSED),
		errors.Is(err, syscall.ECONNABORTED), errors.Is(err, syscall.EPIPE),
		errors.Is(err, syscall.ETIMEDOUT), errors.Is(err, context.DeadlineExceeded):
		return ErrorKindTransient, true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if dnsErr.IsTimeout || dnsErr.IsTemporary {
			return ErrorKindTransient, true
		}
		return 0, false
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return ErrorKindTransient, true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return ErrorKindTransient, true
	}
	msg := strings.ToLower(err.Error())
	for _, p := range transportPhrases {
		if strings.Contains(msg, p) {
			return ErrorKindTransient, true
		}
	}
	return 0, false
}

// ── Structured error types ───────────────────────────────────────────

// ProviderError is a rich error from a provider call.
// errors.Is(err, ErrProviderFailed) returns true, and so does errors.Is with
// the sentinel for Kind (for example ErrContextLength).
type ProviderError struct {
	Provider string    // provider name (e.g. "ollama", "openai")
	Model    string    // model that was called
	Kind     ErrorKind // cause class; Kind.Transient() decides retries
	Code     int       // HTTP status code, 0 if not applicable
	Err      error     // underlying error
	// RetryAfter is the delay the provider asked for before the next attempt,
	// 0 when it did not say. Retry policies wait at least this long.
	RetryAfter time.Duration
}

func (e *ProviderError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("provider %s (model %s, status %d): %v", e.Provider, e.Model, e.Code, e.Err)
	}
	return fmt.Sprintf("provider %s (model %s): %v", e.Provider, e.Model, e.Err)
}

func (e *ProviderError) Unwrap() error { return e.Err }

// ErrorKind implements KindReporter.
func (e *ProviderError) ErrorKind() ErrorKind { return e.Kind }

func (e *ProviderError) Is(target error) bool {
	if target == ErrProviderFailed {
		return true
	}
	s := e.Kind.sentinel()
	return s != nil && target == s
}

// ResponseTruncatedError reports that the model stopped at its output limit.
// errors.Is(err, ErrResponseTruncated) returns true.
type ResponseTruncatedError struct {
	FinishReason    string // FinishReasonMaxTokens for an output-limit stop, whatever the provider called it
	OutputTokens    int    // tokens produced, 0 when unknown
	MaxOutputTokens int    // configured limit, 0 when unknown
}

func (e *ResponseTruncatedError) Error() string {
	msg := "response truncated"
	if e.FinishReason != "" {
		msg += " (finish reason " + e.FinishReason + ")"
	}
	if e.MaxOutputTokens > 0 {
		msg += fmt.Sprintf(": %d of %d output tokens", e.OutputTokens, e.MaxOutputTokens)
	}
	return msg
}

func (e *ResponseTruncatedError) Is(target error) bool { return target == ErrResponseTruncated }

// ErrorKind implements KindReporter.
func (e *ResponseTruncatedError) ErrorKind() ErrorKind { return ErrorKindTruncated }

// IsTruncationFinishReason reports whether a provider finish reason means the
// output token limit cut the response short.
func IsTruncationFinishReason(reason string) bool {
	switch strings.ToLower(reason) {
	case "max_tokens", "length", "max_output_tokens":
		return true
	default:
		return false
	}
}

// finishContentFilter is OpenAI's content-filter finish reason, which is
// also the wire name of ErrorKindContentFilter.
const finishContentFilter = "content_filter"

// IsContentFilterFinishReason reports whether a provider finish reason means
// the provider's safety system stopped the response or the model refused it:
// OpenAI's "content_filter", Anthropic's "refusal", and Gemini's safety,
// blocklist, prohibited-content, personal-data and recitation reasons.
func IsContentFilterFinishReason(reason string) bool {
	switch strings.ToLower(reason) {
	case finishContentFilter, "refusal",
		"safety", "prohibited_content", "blocklist", "spii", "recitation",
		"image_safety", "image_prohibited_content", "image_recitation":
		return true
	default:
		return false
	}
}

// FallbackError is returned when every member of a fallback chain or router
// failed. Errors holds every attempt, in order.
//
// It is classified by its last attempt, the one that ended the chain:
// Unwrap returns that attempt alone, so errors.Is, errors.As, KindOf,
// IsTransient and RetryAfter all answer for it, and an earlier attempt that
// was rate limited does not make the whole failure look rate limited. Read
// Errors for the earlier attempts. errors.Is(err, ErrProviderFailed) is
// always true.
type FallbackError struct {
	Errors []error // one per provider attempted, in order
}

func (e *FallbackError) Error() string {
	return fmt.Sprintf("all %d providers failed: %v", len(e.Errors), errors.Join(e.Errors...))
}

// Unwrap returns the last attempt, or nil when there was none.
func (e *FallbackError) Unwrap() error {
	if len(e.Errors) == 0 {
		return nil
	}
	return e.Errors[len(e.Errors)-1]
}

// Is matches ErrProviderFailed.
func (e *FallbackError) Is(target error) bool { return target == ErrProviderFailed }

// ErrorKind implements KindReporter with the last attempt's kind.
func (e *FallbackError) ErrorKind() ErrorKind { return KindOf(e.Unwrap()) }

// RetryError is returned when all retry attempts are exhausted.
type RetryError struct {
	Attempts int
	Last     error // the final error
}

func (e *RetryError) Error() string {
	return fmt.Sprintf("failed after %d attempts: %v", e.Attempts, e.Last)
}

func (e *RetryError) Unwrap() error { return e.Last }

// ErrorKind implements KindReporter with the final attempt's kind.
func (e *RetryError) ErrorKind() ErrorKind { return KindOf(e.Last) }
