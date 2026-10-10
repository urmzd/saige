package google

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/provider/internal/streamcheck"
	"github.com/urmzd/saige/agent/types"
)

// sseTransport answers every request with the given server-sent event lines.
type sseTransport struct{ events []string }

func (t sseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var b strings.Builder
	for _, e := range t.events {
		b.WriteString("data: " + e + "\n\n")
	}
	return &http.Response{StatusCode: http.StatusOK, Request: req,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   io.NopCloser(strings.NewReader(b.String()))}, nil
}

func TestStreamEndStates(t *testing.T) {
	for _, tc := range []struct {
		name     string
		events   []string
		wantText string
		check    func(error) bool
	}{
		{
			name:     "complete",
			events:   []string{`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}]}`},
			wantText: "hi",
		},
		{
			name:   "prompt blocked",
			events: []string{`{"promptFeedback":{"blockReason":"SAFETY","blockReasonMessage":"unsafe prompt"}}`},
			check: func(err error) bool {
				return types.KindOf(err) == types.ErrorKindContentFilter && strings.Contains(err.Error(), "SAFETY") && strings.Contains(err.Error(), "unsafe prompt") &&
					!errors.Is(err, streamcheck.ErrIncompleteStream) && !types.IsTransient(err)
			},
		},
		{
			name:     "safety stop",
			events:   []string{`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"SAFETY"}]}`},
			wantText: "hi",
			check:    func(err error) bool { return types.IsContentFilter(err) && !types.IsTransient(err) },
		},
		{
			name:     "recitation stop",
			events:   []string{`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"RECITATION"}]}`},
			wantText: "hi",
			check:    func(err error) bool { return types.IsContentFilter(err) },
		},
		{
			name:     "no finish reason",
			events:   []string{`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]}}]}`},
			wantText: "hi",
			check:    func(err error) bool { return errors.Is(err, streamcheck.ErrIncompleteStream) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := New(context.Background(), Config{APIKey: "test-key", Model: "gemini-2.5-flash"},
				WithHTTPClient(&http.Client{Transport: sseTransport{events: tc.events}}))
			if err != nil {
				t.Fatal(err)
			}
			ch, err := a.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("hello"))}})
			if err != nil {
				t.Fatal(err)
			}
			var text string
			var streamErr error
			var all []types.Delta
			for d := range ch {
				all = append(all, d)
				switch v := d.(type) {
				case types.PartDelta:
					text += v.Text
				case types.ErrorDelta:
					streamErr = v.Error
				}
			}
			streamcheck.RunPartConformance(t, all)
			if text != tc.wantText {
				t.Fatalf("text = %q, want %q", text, tc.wantText)
			}
			if tc.check == nil {
				if streamErr != nil {
					t.Fatalf("unexpected error: %v", streamErr)
				}
				return
			}
			if streamErr == nil || !tc.check(streamErr) {
				t.Fatalf("error = %v", streamErr)
			}
		})
	}
}
