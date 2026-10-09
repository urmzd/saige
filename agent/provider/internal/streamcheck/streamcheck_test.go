package streamcheck

import (
	"context"
	"errors"
	"io"
	"net/http"
	"syscall"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/types"
)

func TestDecodeArguments(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     string
		wantErr bool
		wantLen int
	}{
		{"empty is no arguments", "", false, 0},
		{"whitespace is no arguments", "  \n", false, 0},
		{"null is no arguments", "null", false, 0},
		{"object", `{"path":"a","n":2}`, false, 2},
		{"truncated object", `{"path":"a`, true, 0},
		{"missing brace", `{"n": 12`, true, 0},
		{"array is not arguments", `[1,2]`, true, 0},
		{"trailing garbage", `{"a":1}}`, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, err := DecodeArguments(tc.raw)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				if args != nil {
					t.Fatalf("args = %v on error, want nil", args)
				}
				return
			}
			if args == nil || len(args) != tc.wantLen {
				t.Fatalf("args = %#v, want %d non-nil entries", args, tc.wantLen)
			}
		})
	}
}

func TestArgsFailureError(t *testing.T) {
	for _, tc := range []struct {
		name          string
		finish        string
		wantTruncated bool
	}{
		{"anthropic max tokens", "max_tokens", true},
		{"openai length", "length", true},
		{"gemini max tokens", "MAX_TOKENS", true},
		{"clean stop means malformed JSON", "end_turn", false},
		{"missing reason means malformed JSON", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f ArgsFailure
			if !f.Set("call_1", "write", errors.New("unexpected end of JSON input")) {
				t.Fatal("first failure not recorded")
			}
			if f.Set("call_2", "read", errors.New("second")) {
				t.Fatal("second failure replaced the first")
			}
			err := f.Error("p", "m", tc.finish, 10, 10)
			if got := types.IsTruncated(err); got != tc.wantTruncated {
				t.Fatalf("IsTruncated = %v, want %v (%v)", got, tc.wantTruncated, err)
			}
			if types.IsTransient(err) {
				t.Fatal("argument failures must not be retried")
			}
			if err.Model != "m" || err.Provider != "p" {
				t.Fatalf("provider/model not set: %+v", err)
			}
			var te *types.ResponseTruncatedError
			if tc.wantTruncated && (!errors.As(err, &te) || te.FinishReason != types.FinishReasonMaxTokens || te.MaxOutputTokens != 10) {
				t.Fatalf("truncation detail missing: %v", err)
			}
		})
	}
}

func TestStreamError(t *testing.T) {
	for _, tc := range []struct {
		name         string
		err          error
		beforeOutput bool
		want         types.ErrorKind
	}{
		{"reset before output", syscall.ECONNRESET, true, types.ErrorKindTransient},
		{"unexpected EOF before output", io.ErrUnexpectedEOF, true, types.ErrorKindTransient},
		{"incomplete stream before output", ErrIncompleteStream, true, types.ErrorKindTransient},
		{"deadline before output", context.DeadlineExceeded, true, types.ErrorKindTransient},
		{"cancel is never transient", context.Canceled, true, types.ErrorKindPermanent},
		{"reset after output", syscall.ECONNRESET, false, types.ErrorKindPermanent},
		{"unknown error", errors.New("boom"), true, types.ErrorKindPermanent},
		{"context overflow message", errors.New("the prompt is too long for this model"), false, types.ErrorKindContextLength},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := StreamError("p", "m", tc.err, tc.beforeOutput)
			if err.Kind != tc.want {
				t.Fatalf("kind = %v, want %v", err.Kind, tc.want)
			}
			if !errors.Is(err, tc.err) {
				t.Fatal("cause not wrapped")
			}
		})
	}
}

func TestEventError(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		want    types.ErrorKind
	}{
		{"anthropic overloaded", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, types.ErrorKindUnavailable},
		{"anthropic rate limit", `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, types.ErrorKindRateLimit},
		{"anthropic context", `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 210000 tokens"}}`, types.ErrorKindContextLength},
		{"openai inner object", `{"message":"try again","type":"server_error","code":null}`, types.ErrorKindUnavailable},
		{"openai code", `{"message":"too long","type":"invalid_request_error","code":"context_length_exceeded"}`, types.ErrorKindContextLength},
		{"unparseable", `not json`, types.ErrorKindPermanent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := EventError("p", "m", tc.payload, nil).Kind; got != tc.want {
				t.Fatalf("kind = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHTTPError(t *testing.T) {
	for _, tc := range []struct {
		name      string
		code      int
		header    http.Header
		message   string
		wantKind  types.ErrorKind
		wantAfter time.Duration
	}{
		{"rate limit with retry after", 429, http.Header{"Retry-After": {"7"}}, "", types.ErrorKindRateLimit, 7 * time.Second},
		{"overloaded with retry after ms", 529, http.Header{"Retry-After-Ms": {"250"}}, "", types.ErrorKindUnavailable, 250 * time.Millisecond},
		{"bad request", 400, nil, "bad field", types.ErrorKindInvalidRequest, 0},
		{"context window", 400, nil, "This model's maximum context length is 8192 tokens", types.ErrorKindContextLength, 0},
		{"auth ignores retry after", 401, http.Header{"Retry-After": {"7"}}, "", types.ErrorKindAuth, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := HTTPError("p", "m", tc.code, tc.header, tc.message, errors.New("x"))
			if err.Kind != tc.wantKind || err.RetryAfter != tc.wantAfter || err.Code != tc.code || err.Model != "m" {
				t.Fatalf("got %+v", err)
			}
		})
	}
}
