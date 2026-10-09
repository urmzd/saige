package google

import (
	"errors"
	"fmt"
	"syscall"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/types"
	"google.golang.org/genai"
)

func TestClassifyGoogleError(t *testing.T) {
	retryInfo := []map[string]any{{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "7s"}}
	for _, tc := range []struct {
		name         string
		err          error
		beforeOutput bool
		wantKind     types.ErrorKind
		wantAfter    time.Duration
	}{
		{"quota with retry info", genai.APIError{Code: 429, Status: "RESOURCE_EXHAUSTED", Details: retryInfo}, true, types.ErrorKindRateLimit, 7 * time.Second},
		{"wrapped api error", fmt.Errorf("stream: %w", genai.APIError{Code: 503, Status: "UNAVAILABLE"}), true, types.ErrorKindUnavailable, 0},
		{"context window", genai.APIError{Code: 400, Status: "INVALID_ARGUMENT", Message: "The input token count exceeds the maximum number of tokens allowed; reduce the length"}, true, types.ErrorKindContextLength, 0},
		{"invalid request ignores retry info", genai.APIError{Code: 400, Details: retryInfo}, true, types.ErrorKindInvalidRequest, 0},
		{"invalid api key", genai.APIError{Code: 400, Status: "INVALID_ARGUMENT", Message: "API key not valid. Please pass a valid API key.", Details: []map[string]any{{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "API_KEY_INVALID", "domain": "googleapis.com"}}}, true, types.ErrorKindAuth, 0},
		{"expired api key", genai.APIError{Code: 400, Status: "INVALID_ARGUMENT", Details: []map[string]any{{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "API_KEY_EXPIRED"}}}, true, types.ErrorKindAuth, 0},
		{"other error info reason", genai.APIError{Code: 400, Status: "INVALID_ARGUMENT", Details: []map[string]any{{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "FIELD_INVALID"}}}, true, types.ErrorKindInvalidRequest, 0},
		{"reset before output", fmt.Errorf("read: %w", syscall.ECONNRESET), true, types.ErrorKindTransient, 0},
		{"reset after output", fmt.Errorf("read: %w", syscall.ECONNRESET), false, types.ErrorKindPermanent, 0},
		{"unknown", errors.New("boom"), true, types.ErrorKindPermanent, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pe := classifyGoogleError("gemini-2.5-flash", tc.err, tc.beforeOutput)
			if pe.Kind != tc.wantKind || pe.RetryAfter != tc.wantAfter || pe.Model != "gemini-2.5-flash" {
				t.Fatalf("got %+v", pe)
			}
		})
	}
}

func TestRetryDelay(t *testing.T) {
	for _, tc := range []struct {
		name    string
		details []map[string]any
		want    time.Duration
	}{
		{"none", nil, 0},
		{"fractional", []map[string]any{{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "1.5s"}}, 1500 * time.Millisecond},
		{"other detail ignored", []map[string]any{{"@type": "type.googleapis.com/google.rpc.QuotaFailure", "retryDelay": "9s"}}, 0},
		{"malformed", []map[string]any{{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "soon"}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryDelay(tc.details); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
