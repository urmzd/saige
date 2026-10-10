package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/provider/internal/streamcheck"
	"github.com/urmzd/saige/agent/provider/retry"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

const testModel = "gpt-4o"

// chunk renders one chat.completion.chunk SSE event. choice is the JSON of
// choices[0] without the index, or "" for a chunk with no choices.
func chunk(choice string) string {
	choices := "[]"
	if choice != "" {
		choices = `[{"index":0,` + choice + `}]`
	}
	return fmt.Sprintf(`data: {"id":"c1","object":"chat.completion.chunk","created":0,"model":"gpt-4o","choices":%s}`+"\n\n", choices)
}

func toolDelta(index int, id, name, args string) string {
	head := fmt.Sprintf(`"index":%d`, index)
	if id != "" {
		head += fmt.Sprintf(`,"id":%q,"type":"function"`, id)
	}
	fn := fmt.Sprintf(`"arguments":%q`, args)
	if name != "" {
		fn = fmt.Sprintf(`"name":%q,`, name) + fn
	}
	return chunk(fmt.Sprintf(`"delta":{"tool_calls":[{%s,"function":{%s}}]}`, head, fn))
}

func finish(reason string) string {
	return chunk(fmt.Sprintf(`"delta":{},"finish_reason":%q`, reason))
}

const usageChunk = `data: {"id":"c1","object":"chat.completion.chunk","created":0,"model":"gpt-4o","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}}` + "\n\n"

func sseServer(t *testing.T, hangUp bool, body ...string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join(body, "")))
		if !hangUp {
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		w.(http.Flusher).Flush()
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}
	}))
	t.Cleanup(server.Close)
	return server
}

type streamResult struct {
	deltas []types.Delta
	ends   []types.ToolCallPart
	errs   []error
	usage  bool
}

func run(t *testing.T, a *Adapter, schema *types.ParameterSchema) streamResult {
	t.Helper()
	ch, err := a.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("go"))}, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	var r streamResult
	for d := range ch {
		r.deltas = append(r.deltas, d)
		switch v := d.(type) {
		case types.PartEnd:
			if tc, ok := v.Part.(types.ToolCallPart); ok {
				r.ends = append(r.ends, tc)
			}
		case types.UsageDelta:
			if len(r.errs) == 0 {
				r.usage = true
			}
		case types.ErrorDelta:
			r.errs = append(r.errs, v.Error)
		}
	}
	streamcheck.RunPartConformance(t, r.deltas)
	return r
}

func TestToolArgumentIntegrity(t *testing.T) {
	for _, tc := range []struct {
		name          string
		body          []string
		wantEnds      map[string]string // call ID -> value of the "path" argument
		wantArgsErr   []string          // call IDs that end with ArgumentsError, in order
		wantTruncated bool
		wantErr       bool
	}{
		{
			name: "parallel calls pair with their own arguments",
			body: []string{
				toolDelta(0, "call_a", "read", `{"path":`), toolDelta(0, "", "", `"a.txt"}`),
				toolDelta(1, "call_b", "read", `{"path":"b.txt"}`),
				finish("tool_calls"), usageChunk,
			},
			wantEnds: map[string]string{"call_a": "a.txt", "call_b": "b.txt"},
		},
		{
			name:          "length inside a tool call is a truncation",
			body:          []string{toolDelta(0, "call_a", "write", `{"path":"a.t`), finish("length"), usageChunk},
			wantTruncated: true, wantErr: true,
		},
		{
			name:        "malformed complete arguments end the call with an error",
			body:        []string{toolDelta(0, "call_a", "write", `{"path":}`), finish("tool_calls"), usageChunk},
			wantArgsErr: []string{"call_a"},
		},
		{
			name: "malformed earlier call ends with an error before the next starts",
			body: []string{
				toolDelta(0, "call_a", "write", `{"path":}`),
				toolDelta(1, "call_b", "read", `{"path":"b.txt"}`),
				finish("tool_calls"), usageChunk,
			},
			wantEnds:    map[string]string{"call_b": "b.txt"},
			wantArgsErr: []string{"call_a"},
		},
		{
			name:    "stream without a finish reason leaves the call open",
			body:    []string{toolDelta(0, "call_a", "write", `{"path":"a`), usageChunk},
			wantErr: true,
		},
		{
			name:     "a call with no arguments gets an empty map",
			body:     []string{toolDelta(0, "call_a", "list", ``), finish("tool_calls"), usageChunk},
			wantEnds: map[string]string{"call_a": ""},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := run(t, must.Get(New(Config{APIKey: "test", Model: types.ModelID(testModel)}, WithBaseURL(sseServer(t, false, tc.body...).URL))), nil)
			var argsErrs []string
			var ends []types.ToolCallPart
			for _, end := range r.ends {
				if end.ArgumentsError != "" {
					if end.Arguments != nil {
						t.Fatalf("end %+v has both arguments and an error", end)
					}
					argsErrs = append(argsErrs, end.ID)
					continue
				}
				ends = append(ends, end)
			}
			if strings.Join(argsErrs, ",") != strings.Join(tc.wantArgsErr, ",") {
				t.Fatalf("argument errors on %v, want %v", argsErrs, tc.wantArgsErr)
			}
			if len(ends) != len(tc.wantEnds) {
				t.Fatalf("ends = %+v, want %d", ends, len(tc.wantEnds))
			}
			// Every call closes before the next one starts.
			open, openIndex := "", -1
			for _, d := range r.deltas {
				switch v := d.(type) {
				case types.PartStart:
					if v.Kind != types.KindToolCall {
						continue
					}
					if open != "" {
						t.Fatalf("call %s started while %s was open", v.ID, open)
					}
					open, openIndex = v.ID, v.Index
				case types.PartEnd:
					tc, ok := v.Part.(types.ToolCallPart)
					if !ok {
						continue
					}
					if tc.ID != open || v.Index != openIndex {
						t.Fatalf("end for %s while %q was open", tc.ID, open)
					}
					open, openIndex = "", -1
				}
			}
			for _, end := range ends {
				want, ok := tc.wantEnds[end.ID]
				if !ok || end.Arguments == nil {
					t.Fatalf("unexpected end %+v", end)
				}
				if got, _ := end.Arguments["path"].(string); got != want {
					t.Fatalf("call %s path = %q, want %q", end.ID, got, want)
				}
			}
			starts := map[int]bool{}
			for _, d := range r.deltas {
				switch v := d.(type) {
				case types.PartStart:
					starts[v.Index] = v.Kind == types.KindToolCall
				case types.PartDelta:
					if v.Args != "" && !starts[v.Index] {
						t.Fatalf("argument delta for unknown call at %d", v.Index)
					}
				}
			}
			if (len(r.errs) > 0) != tc.wantErr {
				t.Fatalf("errors = %v, wantErr %v", r.errs, tc.wantErr)
			}
			if tc.wantErr {
				if got := types.IsTruncated(r.errs[0]); got != tc.wantTruncated {
					t.Fatalf("IsTruncated = %v, want %v: %v", got, tc.wantTruncated, r.errs[0])
				}
				if !r.usage {
					t.Fatal("usage must arrive before the error")
				}
			}
		})
	}
}

func TestTruncatedStructuredOutput(t *testing.T) {
	body := []string{chunk(`"delta":{"content":"{\"answer\":\"par"}`), finish("length"), usageChunk}
	r := run(t, must.Get(New(Config{APIKey: "test", Model: types.ModelID(testModel)}, WithBaseURL(sseServer(t, false, body...).URL))), &types.ParameterSchema{Type: "object"})
	if len(r.errs) != 1 || !types.IsTruncated(r.errs[0]) {
		t.Fatalf("errors = %v, want one truncation", r.errs)
	}
}

func TestStreamFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name          string
		hangUp        bool
		body          []string
		wantTransient bool
	}{
		{"dropped before content", true, nil, true},
		{"dropped after content", true, []string{chunk(`"delta":{"content":"hel"}`)}, false},
		{"done without a finish reason", false, []string{chunk(`"delta":{"role":"assistant"}`)}, true},
		{"server error event", false, []string{`data: {"error":{"message":"try again","type":"server_error"}}` + "\n\n"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := run(t, must.Get(New(Config{APIKey: "test", Model: types.ModelID(testModel)}, WithBaseURL(sseServer(t, tc.hangUp, tc.body...).URL))), nil)
			if len(r.errs) != 1 {
				t.Fatalf("errors = %v, want 1", r.errs)
			}
			if types.IsTransient(r.errs[0]) != tc.wantTransient {
				t.Fatalf("transient = %v, want %v: %v", types.IsTransient(r.errs[0]), tc.wantTransient, r.errs[0])
			}
			var pe *types.ProviderError
			if !errors.As(r.errs[0], &pe) || pe.Model != testModel {
				t.Fatalf("model not recorded: %v", r.errs[0])
			}
		})
	}
}

func TestHTTPErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		header    map[string]string
		body      string
		wantKind  types.ErrorKind
		wantAfter time.Duration
	}{
		{"context length", 400, nil, `{"error":{"message":"This model's maximum context length is 128000 tokens.","type":"invalid_request_error","code":"context_length_exceeded"}}`, types.ErrorKindContextLength, 0},
		{"rate limited", 429, map[string]string{"Retry-After": "7"}, `{"error":{"message":"slow","type":"requests","code":"rate_limit_exceeded"}}`, types.ErrorKindRateLimit, 7 * time.Second},
		{"bad request", 400, nil, `{"error":{"message":"bad field","type":"invalid_request_error","code":null}}`, types.ErrorKindInvalidRequest, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.header {
					w.Header().Set(k, v)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			r := run(t, must.Get(New(Config{APIKey: "test", Model: types.ModelID(testModel)}, WithBaseURL(server.URL))), nil)
			if len(r.errs) != 1 {
				t.Fatalf("errors = %v", r.errs)
			}
			if types.KindOf(r.errs[0]) != tc.wantKind || types.RetryAfter(r.errs[0]) != tc.wantAfter {
				t.Fatalf("kind = %v after = %v: %v", types.KindOf(r.errs[0]), types.RetryAfter(r.errs[0]), r.errs[0])
			}
			// The SDK's own message is a bare status line; the API's
			// message must still reach the caller, without the request URL.
			var body struct{ Error struct{ Message string } }
			_ = json.Unmarshal([]byte(tc.body), &body)
			if msg := r.errs[0].Error(); !strings.Contains(msg, body.Error.Message) || strings.Contains(msg, server.URL) {
				t.Fatalf("error = %q, want the API message %q and no URL", msg, body.Error.Message)
			}
		})
	}
}

// TestSDKRetriesDisabledByDefault checks that the retry decorator is the only
// retry layer unless SDK retries are requested explicitly.
func TestSDKRetriesDisabledByDefault(t *testing.T) {
	for _, tc := range []struct {
		name  string
		opts  []Option
		outer int
		want  int32
	}{
		{"bare adapter sends one request", nil, 0, 1},
		{"retry decorator owns every attempt", nil, 3, 3},
		{"explicit SDK retries", []Option{WithMaxRetries(2)}, 0, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After-Ms", "1")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(503)
				_, _ = w.Write([]byte(`{"error":{"message":"down","type":"server_error"}}`))
			}))
			defer server.Close()
			var p types.Provider = must.Get(New(Config{APIKey: "test", Model: types.ModelID(testModel)}, append([]Option{WithBaseURL(server.URL)}, tc.opts...)...))
			if tc.outer > 0 {
				p = must.Get(retry.New(p, retry.Config{MaxAttempts: tc.outer, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}))
			}
			ch, err := p.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("go"))}})
			if err == nil {
				for d := range ch {
					if e, ok := d.(types.ErrorDelta); ok {
						err = e.Error
					}
				}
			}
			if !types.IsUnavailable(err) {
				t.Fatalf("err = %v, want unavailable", err)
			}
			if got := calls.Load(); got != tc.want {
				t.Fatalf("requests = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestEmbedderKeepsSDKRetries checks that a bare embedder still rides out a
// rate limit, since no retry decorator wraps embedders.
func TestEmbedderKeepsSDKRetries(t *testing.T) {
	for _, tc := range []struct {
		name      string
		opts      []Option
		failures  int32
		wantErr   bool
		wantCalls int32
	}{
		{"429 then 200 succeeds", nil, 1, false, 2},
		{"retries disabled", []Option{WithMaxRetries(0)}, 1, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if calls.Add(1) <= tc.failures {
					w.Header().Set("Retry-After-Ms", "1")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = w.Write([]byte(`{"error":{"message":"slow down","type":"rate_limit"}}`))
					return
				}
				_, _ = w.Write([]byte(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.5,0.25]}],"model":"text-embedding-3-small"}`))
			}))
			defer server.Close()
			opts := append([]Option{WithBaseURL(server.URL)}, tc.opts...)
			vecs, err := must.Get(NewEmbedder(Config{APIKey: "test", Model: "text-embedding-3-small"}, opts...)).Embed(context.Background(), []string{"x"})
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && (len(vecs) != 1 || len(vecs[0]) != 2) {
				t.Fatalf("vectors = %v", vecs)
			}
			if got := calls.Load(); got != tc.wantCalls {
				t.Fatalf("requests = %d, want %d", got, tc.wantCalls)
			}
		})
	}
}

// TestContentFilterFinishIsAnError checks that a content_filter finish reason
// ends the stream with a content-filter error instead of a normal answer.
func TestContentFilterFinishIsAnError(t *testing.T) {
	for _, tc := range []struct {
		reason  string
		wantErr bool
	}{
		{"stop", false},
		{"content_filter", true},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			body := []string{chunk(`"delta":{"content":"partial"}`), finish(tc.reason), usageChunk}
			r := run(t, must.Get(New(Config{APIKey: "test", Model: types.ModelID(testModel)}, WithBaseURL(sseServer(t, false, body...).URL))), nil)
			if !tc.wantErr {
				if len(r.errs) != 0 {
					t.Fatalf("errors = %v, want none", r.errs)
				}
				return
			}
			if len(r.errs) != 1 || !types.IsContentFilter(r.errs[0]) || types.IsTransient(r.errs[0]) {
				t.Fatalf("errors = %v, want one content-filter error", r.errs)
			}
		})
	}
}
