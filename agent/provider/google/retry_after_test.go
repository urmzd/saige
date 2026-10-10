package google

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// A 429 whose body carries no RetryInfo still reports the delay its
// Retry-After header asked for.
func TestRateLimitHonorsRetryAfterHeader(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want time.Duration
	}{
		{"header only", `{"error":{"code":429,"message":"quota","status":"RESOURCE_EXHAUSTED"}}`, 12 * time.Second},
		{"longer retry info wins", `{"error":{"code":429,"message":"quota","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"30s"}]}}`, 30 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusTooManyRequests, Request: req,
					Header: http.Header{"Content-Type": []string{"application/json"}, "Retry-After": []string{"12"}},
					Body:   io.NopCloser(strings.NewReader(tc.body))}, nil
			})
			a, err := NewAdapter(context.Background(), "k", "gemini-2.5-flash", WithHTTPClient(&http.Client{Transport: transport}))
			if err != nil {
				t.Fatal(err)
			}
			ch, err := a.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("hi"))}})
			if err != nil {
				t.Fatal(err)
			}
			var got error
			for d := range ch {
				if e, ok := d.(types.ErrorDelta); ok {
					got = e.Error
				}
			}
			if !types.IsTransient(got) || types.RetryAfter(got) != tc.want {
				t.Fatalf("err = %v, retry after %v, want %v", got, types.RetryAfter(got), tc.want)
			}
		})
	}
}
