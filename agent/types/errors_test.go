package types

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestErrorKindTransient(t *testing.T) {
	tests := []struct {
		kind ErrorKind
		want bool
	}{
		{ErrorKindTransient, true},
		{ErrorKindPermanent, false},
		{ErrorKindContextLength, false},
		{ErrorKindContentFilter, false},
		{ErrorKindAuth, false},
		{ErrorKindRateLimit, true},
		{ErrorKindUnavailable, true},
		{ErrorKindInvalidRequest, false},
		{ErrorKindTruncated, false},
	}
	for _, tt := range tests {
		t.Run(tt.kind.String(), func(t *testing.T) {
			if got := tt.kind.Transient(); got != tt.want {
				t.Errorf("Transient() = %v, want %v", got, tt.want)
			}
			pe := &ProviderError{Kind: tt.kind, Err: errors.New("x")}
			if got := IsTransient(pe); got != tt.want {
				t.Errorf("IsTransient(ProviderError) = %v, want %v", got, tt.want)
			}
			if got := ParseErrorKind(tt.kind.String()); got != tt.kind {
				t.Errorf("ParseErrorKind(%q) = %v", tt.kind.String(), got)
			}
		})
	}
	if got := ParseErrorKind("from_the_future"); got != ErrorKindPermanent {
		t.Errorf("unknown kind parsed as %v, want permanent", got)
	}
	if got := ErrorKind(99).String(); got != "permanent" {
		t.Errorf("out-of-range kind String() = %q", got)
	}
}

func TestClassifyHTTPStatusKind(t *testing.T) {
	tests := []struct {
		code int
		want ErrorKind
	}{
		{200, ErrorKindPermanent},
		{400, ErrorKindInvalidRequest},
		{401, ErrorKindAuth},
		{403, ErrorKindAuth},
		{404, ErrorKindInvalidRequest},
		{408, ErrorKindTransient},
		{413, ErrorKindInvalidRequest},
		{422, ErrorKindInvalidRequest},
		{429, ErrorKindRateLimit},
		{500, ErrorKindUnavailable},
		{501, ErrorKindTransient},
		{503, ErrorKindUnavailable},
		{529, ErrorKindUnavailable},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.code), func(t *testing.T) {
			got := ClassifyHTTPStatusKind(tt.code)
			if got != tt.want {
				t.Errorf("ClassifyHTTPStatusKind(%d) = %v, want %v", tt.code, got, tt.want)
			}
			if got.Transient() != ClassifyHTTPStatus(tt.code).Transient() {
				t.Errorf("fine and coarse classes disagree on Transient for %d", tt.code)
			}
		})
	}
}

func TestClassifyHTTPError(t *testing.T) {
	retry := http.Header{}
	retry.Set("Retry-After", "3")
	tests := []struct {
		name      string
		code      int
		header    http.Header
		msg       string
		wantKind  ErrorKind
		wantDelay time.Duration
	}{
		{"context length on 400", 400, nil, "This model's maximum context length is 8192 tokens", ErrorKindContextLength, 0},
		{"openai code", 400, nil, `{"code":"context_length_exceeded"}`, ErrorKindContextLength, 0},
		{"anthropic prompt too long", 400, nil, "prompt is too long: 210000 tokens > 200000 maximum", ErrorKindContextLength, 0},
		{"content filter", 400, nil, "The response was filtered due to the prompt triggering content management policy", ErrorKindContentFilter, 0},
		{"plain bad request", 400, nil, "invalid parameter", ErrorKindInvalidRequest, 0},
		{"rate limit keeps kind and delay", 429, retry, "too many tokens per minute", ErrorKindRateLimit, 3 * time.Second},
		{"unavailable with delay", 503, retry, "", ErrorKindUnavailable, 3 * time.Second},
		{"auth ignores delay", 401, retry, "", ErrorKindAuth, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, delay := ClassifyHTTPError(tt.code, tt.header, tt.msg)
			if kind != tt.wantKind || delay != tt.wantDelay {
				t.Errorf("got (%v, %v), want (%v, %v)", kind, delay, tt.wantKind, tt.wantDelay)
			}
		})
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"5", 5 * time.Second},
		{"1.5", 1500 * time.Millisecond},
		{"0", 0},
		{"-3", 0},
		{"soon", 0},
		{now.Add(10 * time.Second).Format(http.TimeFormat), 10 * time.Second},
		{now.Add(-10 * time.Second).Format(http.TimeFormat), 0},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := ParseRetryAfter(tt.in, now); got != tt.want {
				t.Errorf("ParseRetryAfter(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestRetryAfterFromHeader(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		set  map[string]string
		want time.Duration
	}{
		{"none", nil, 0},
		{"seconds", map[string]string{"Retry-After": "2"}, 2 * time.Second},
		{"milliseconds win", map[string]string{"Retry-After-Ms": "250", "Retry-After": "2"}, 250 * time.Millisecond},
		{"bad milliseconds fall back", map[string]string{"Retry-After-Ms": "x", "Retry-After": "2"}, 2 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tt.set {
				h.Set(k, v)
			}
			if got := RetryAfterFromHeader(h, now); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
	if got := RetryAfterFromHeader(nil, now); got != 0 {
		t.Errorf("nil header = %v", got)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "op timed out" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestClassifyTransportError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"eof", io.EOF, true},
		{"unexpected eof wrapped", fmt.Errorf("read body: %w", io.ErrUnexpectedEOF), true},
		{"reset", &net.OpError{Op: "read", Err: &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}}, true},
		{"refused dial", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, true},
		{"net timeout", timeoutErr{}, true},
		{"deadline before first token", context.DeadlineExceeded, true},
		{"caller cancel", context.Canceled, false},
		{"dns not found", &net.DNSError{Err: "no such host", Name: "x", IsNotFound: true}, false},
		{"dns temporary", &net.DNSError{Err: "server misbehaving", Name: "x", IsTemporary: true}, true},
		{"sdk text", errors.New(`Post "https://api": http2: server sent GOAWAY and closed the connection`), true},
		{"other", errors.New("invalid api key"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, ok := ClassifyTransportError(tt.err)
			if ok != tt.want {
				t.Fatalf("ok = %v, want %v", ok, tt.want)
			}
			if ok && !kind.Transient() {
				t.Errorf("kind %v is not transient", kind)
			}
		})
	}
}

func TestProviderErrorSentinels(t *testing.T) {
	tests := []struct {
		kind  ErrorKind
		match error
		check func(error) bool
	}{
		{ErrorKindContextLength, ErrContextLength, IsContextLength},
		{ErrorKindContentFilter, ErrContentFiltered, IsContentFilter},
		{ErrorKindAuth, ErrAuth, IsAuth},
		{ErrorKindRateLimit, ErrRateLimited, IsRateLimit},
		{ErrorKindUnavailable, ErrUnavailable, IsUnavailable},
		{ErrorKindInvalidRequest, ErrInvalidRequest, nil},
		{ErrorKindTruncated, ErrResponseTruncated, IsTruncated},
	}
	for _, tt := range tests {
		t.Run(tt.kind.String(), func(t *testing.T) {
			err := fmt.Errorf("turn: %w", &ProviderError{Provider: "p", Kind: tt.kind, Err: errors.New("x")})
			if !errors.Is(err, tt.match) {
				t.Errorf("errors.Is(err, %v) = false", tt.match)
			}
			if !errors.Is(err, ErrProviderFailed) {
				t.Error("lost ErrProviderFailed")
			}
			if tt.check != nil && !tt.check(err) {
				t.Error("helper returned false")
			}
			if KindOf(err) != tt.kind {
				t.Errorf("KindOf = %v", KindOf(err))
			}
			for _, other := range tests {
				if other.kind != tt.kind && errors.Is(err, other.match) {
					t.Errorf("also matched %v", other.match)
				}
			}
		})
	}
	if errors.Is(&ProviderError{Kind: ErrorKindTransient, Err: errors.New("x")}, ErrContextLength) {
		t.Error("transient error matched a kind sentinel")
	}
}

func TestKindOfAndRetryAfterThroughFallback(t *testing.T) {
	fe := &FallbackError{Errors: []error{
		&ProviderError{Kind: ErrorKindAuth, Err: errors.New("a")},
		&ProviderError{Kind: ErrorKindRateLimit, RetryAfter: 4 * time.Second, Err: errors.New("b")},
	}}
	if got := KindOf(fe); got != ErrorKindRateLimit {
		t.Errorf("KindOf = %v, want rate_limit", got)
	}
	if got := RetryAfter(fe); got != 4*time.Second {
		t.Errorf("RetryAfter = %v", got)
	}
	if got := KindOf(errors.New("plain")); got != ErrorKindPermanent {
		t.Errorf("KindOf(plain) = %v", got)
	}
}

func TestResponseTruncatedError(t *testing.T) {
	err := fmt.Errorf("step: %w", &ResponseTruncatedError{FinishReason: "max_tokens", OutputTokens: 100, MaxOutputTokens: 100})
	if !errors.Is(err, ErrResponseTruncated) || !IsTruncated(err) {
		t.Fatal("does not match ErrResponseTruncated")
	}
	if KindOf(err) != ErrorKindTruncated || IsTransient(err) {
		t.Errorf("KindOf = %v, IsTransient = %v", KindOf(err), IsTransient(err))
	}
	for reason, want := range map[string]bool{"max_tokens": true, "length": true, "MAX_TOKENS": true, "stop": false, "": false} {
		if got := IsTruncationFinishReason(reason); got != want {
			t.Errorf("IsTruncationFinishReason(%q) = %v", reason, got)
		}
	}
}

func TestIsContentFilterFinishReason(t *testing.T) {
	for reason, want := range map[string]bool{
		"content_filter": true, "refusal": true, "SAFETY": true, "PROHIBITED_CONTENT": true,
		"BLOCKLIST": true, "SPII": true, "RECITATION": true,
		"stop": false, "end_turn": false, "STOP": false, "length": false, "max_tokens": false, "": false,
	} {
		if got := IsContentFilterFinishReason(reason); got != want {
			t.Errorf("IsContentFilterFinishReason(%q) = %v, want %v", reason, got, want)
		}
	}
}
