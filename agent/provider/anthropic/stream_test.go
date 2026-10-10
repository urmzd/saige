package anthropic

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/provider/internal/streamcheck"
	"github.com/urmzd/saige/agent/provider/retry"
	"github.com/urmzd/saige/agent/types"
)

const testModel = "claude-sonnet-4-5"

type sseEvent struct{ kind, data string }

const (
	evStart     = `{"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[],"usage":{"input_tokens":10,"output_tokens":1}}}`
	evStop      = `{"type":"message_stop"}`
	evToolStart = `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"write_file","input":{}}}`
	evTextStart = `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`
	evBlockStop = `{"type":"content_block_stop","index":0}`
)

func evArgs(partial string) string {
	return fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":%q}}`, partial)
}

func evText(text string) string {
	return fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, text)
}

func evMessageDelta(stop string, out int) string {
	return fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q},"usage":{"output_tokens":%d}}`, stop, out)
}

// sseServer replies with the events. With hangUp set it then drops the
// connection without finishing the response.
func sseServer(t *testing.T, hangUp bool, events ...sseEvent) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range events {
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.kind, e.data)
		}
		w.(http.Flusher).Flush()
		if hangUp {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}
	}))
	t.Cleanup(server.Close)
	return server
}

type streamResult struct {
	deltas []types.Delta
	ends   []types.ToolCallPart
	errs   []error
	usage  bool // a usage delta arrived before the first error
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
		events        []sseEvent
		wantEnds      int
		wantArgsErr   int
		wantTruncated bool
		wantErr       bool
	}{
		{
			name: "complete arguments close the call",
			events: []sseEvent{{"message_start", evStart}, {"content_block_start", evToolStart},
				{"content_block_delta", evArgs(`{"path":"a.txt",`)}, {"content_block_delta", evArgs(`"n":3}`)},
				{"content_block_stop", evBlockStop}, {"message_delta", evMessageDelta("tool_use", 20)}, {"message_stop", evStop}},
			wantEnds: 1,
		},
		{
			name: "max tokens inside a tool call is a truncation",
			events: []sseEvent{{"message_start", evStart}, {"content_block_start", evToolStart},
				{"content_block_delta", evArgs(`{"path":"a.t`)},
				{"content_block_stop", evBlockStop}, {"message_delta", evMessageDelta("max_tokens", 4096)}, {"message_stop", evStop}},
			wantTruncated: true, wantErr: true,
		},
		{
			name: "malformed complete arguments end the call with an error",
			events: []sseEvent{{"message_start", evStart}, {"content_block_start", evToolStart},
				{"content_block_delta", evArgs(`{"path":}`)},
				{"content_block_stop", evBlockStop}, {"message_delta", evMessageDelta("tool_use", 20)}, {"message_stop", evStop}},
			wantArgsErr: 1,
		},
		{
			name: "malformed call ends with an error before the next block starts",
			events: []sseEvent{{"message_start", evStart}, {"content_block_start", evToolStart},
				{"content_block_delta", evArgs(`{"path":}`)}, {"content_block_stop", evBlockStop},
				{"content_block_start", evTextStart}, {"content_block_delta", evText("done")}, {"content_block_stop", evBlockStop},
				{"message_delta", evMessageDelta("end_turn", 20)}, {"message_stop", evStop}},
			wantArgsErr: 1,
		},
		{
			name: "stream that ends inside a malformed call is incomplete",
			events: []sseEvent{{"message_start", evStart}, {"content_block_start", evToolStart},
				{"content_block_delta", evArgs(`{"path":"a`)}, {"content_block_stop", evBlockStop},
				{"message_delta", evMessageDelta("", 20)}},
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := NewAdapter("test", testModel, WithBaseURL(sseServer(t, false, tc.events...).URL))
			r := run(t, a, nil)
			var ends []types.ToolCallPart
			argsErrs := 0
			for _, end := range r.ends {
				if end.ArgumentsError != "" {
					if end.Arguments != nil || end.ID != "toolu_1" {
						t.Fatalf("end = %+v, want ID and nil arguments with the error", end)
					}
					argsErrs++
					continue
				}
				ends = append(ends, end)
			}
			if argsErrs != tc.wantArgsErr {
				t.Fatalf("argument errors = %d, want %d", argsErrs, tc.wantArgsErr)
			}
			// A held call closes before the next block starts.
			open, openIndex := "", -1
			for _, d := range r.deltas {
				switch v := d.(type) {
				case types.PartStart:
					switch v.Kind {
					case types.KindToolCall:
						open, openIndex = v.ID, v.Index
					case types.KindText:
						if open != "" {
							t.Fatalf("text started while call %s was open", open)
						}
					}
				case types.PartEnd:
					if v.Index == openIndex {
						open, openIndex = "", -1
					}
				}
			}
			if len(ends) != tc.wantEnds {
				t.Fatalf("ends = %d, want %d", len(ends), tc.wantEnds)
			}
			for _, end := range ends {
				if end.Arguments == nil || end.ID != "toolu_1" {
					t.Fatalf("end = %+v, want ID and non-nil arguments", end)
				}
				if end.Arguments["n"] != float64(3) {
					t.Fatalf("arguments = %v", end.Arguments)
				}
			}
			if (len(r.errs) > 0) != tc.wantErr {
				t.Fatalf("errors = %v, wantErr %v", r.errs, tc.wantErr)
			}
			if tc.wantErr {
				if got := types.IsTruncated(r.errs[0]); got != tc.wantTruncated {
					t.Fatalf("IsTruncated = %v, want %v: %v", got, tc.wantTruncated, r.errs[0])
				}
				if types.IsTransient(r.errs[0]) {
					t.Fatal("an argument failure after output must not be retried")
				}
				if !r.usage {
					t.Fatal("usage must arrive before the error so tokens are accounted")
				}
			}
			callIndex := map[int]string{}
			for _, d := range r.deltas {
				switch v := d.(type) {
				case types.PartStart:
					callIndex[v.Index] = v.ID
				case types.PartDelta:
					if v.Args != "" && callIndex[v.Index] != "toolu_1" {
						t.Fatalf("argument delta outside the call: %+v", v)
					}
				}
			}
		})
	}
}

func TestTruncatedStructuredOutput(t *testing.T) {
	events := []sseEvent{{"message_start", evStart},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_s","name":"structured_output","input":{}}}`},
		{"content_block_delta", evArgs(`{"answer":"par`)},
		{"content_block_stop", evBlockStop}, {"message_delta", evMessageDelta("max_tokens", 4096)}, {"message_stop", evStop}}
	a := NewAdapter("test", testModel, WithBaseURL(sseServer(t, false, events...).URL))
	r := run(t, a, &types.ParameterSchema{Type: "object", Properties: map[string]types.PropertyDef{"answer": {Type: "string"}}})
	if len(r.errs) != 1 || !types.IsTruncated(r.errs[0]) {
		t.Fatalf("errors = %v, want one truncation", r.errs)
	}
}

func TestStreamFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name          string
		hangUp        bool
		events        []sseEvent
		wantTransient bool
		wantKind      types.ErrorKind
	}{
		{"dropped before content", true, []sseEvent{{"message_start", evStart}}, true, types.ErrorKindTransient},
		{"dropped after content", true, []sseEvent{{"message_start", evStart}, {"content_block_start", evTextStart}, {"content_block_delta", evText("hel")}}, false, types.ErrorKindPermanent},
		{"clean close without message_stop", false, []sseEvent{{"message_start", evStart}}, true, types.ErrorKindTransient},
		{"overloaded error event", false, []sseEvent{{"message_start", evStart}, {"error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`}}, true, types.ErrorKindUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := NewAdapter("test", testModel, WithBaseURL(sseServer(t, tc.hangUp, tc.events...).URL))
			r := run(t, a, nil)
			if len(r.errs) != 1 {
				t.Fatalf("errors = %v, want 1", r.errs)
			}
			err := r.errs[0]
			if types.IsTransient(err) != tc.wantTransient || types.KindOf(err) != tc.wantKind {
				t.Fatalf("kind = %v transient = %v, want %v/%v: %v", types.KindOf(err), types.IsTransient(err), tc.wantKind, tc.wantTransient, err)
			}
			var pe *types.ProviderError
			if !errors.As(err, &pe) || pe.Model != testModel {
				t.Fatalf("model not recorded: %v", err)
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
		{"prompt too long", 400, nil, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 210000 tokens > 200000 maximum"}}`, types.ErrorKindContextLength, 0},
		{"rate limited", 429, map[string]string{"Retry-After": "7"}, `{"type":"error","error":{"type":"rate_limit_error","message":"slow"}}`, types.ErrorKindRateLimit, 7 * time.Second},
		{"bad key", 401, nil, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, types.ErrorKindAuth, 0},
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
			r := run(t, NewAdapter("test", testModel, WithBaseURL(server.URL)), nil)
			if len(r.errs) != 1 {
				t.Fatalf("errors = %v", r.errs)
			}
			if types.KindOf(r.errs[0]) != tc.wantKind || types.RetryAfter(r.errs[0]) != tc.wantAfter {
				t.Fatalf("kind = %v after = %v: %v", types.KindOf(r.errs[0]), types.RetryAfter(r.errs[0]), r.errs[0])
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
		outer int // retry.Provider attempts; 0 = bare adapter
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
				w.WriteHeader(529)
				_, _ = w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))
			}))
			defer server.Close()
			var p types.Provider = NewAdapter("test", testModel, append([]Option{WithBaseURL(server.URL)}, tc.opts...)...)
			if tc.outer > 0 {
				p = retry.New(p, retry.Config{MaxAttempts: tc.outer, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond})
			}
			ch, err := p.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("go"))}})
			if err == nil {
				for d := range ch {
					if e, ok := d.(types.ErrorDelta); ok {
						err = e.Error
					}
				}
			}
			if err == nil || !types.IsUnavailable(err) {
				t.Fatalf("err = %v, want unavailable", err)
			}
			if got := calls.Load(); got != tc.want {
				t.Fatalf("requests = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestStopReasonsThatAreNotAnswers checks that a refusal ends the stream with
// a content-filter error, a paused server tool turn with a permanent error,
// and a response cut off at the context window with a context-length error,
// while a normal end of turn succeeds.
func TestStopReasonsThatAreNotAnswers(t *testing.T) {
	for _, tc := range []struct {
		stop     string
		wantErr  bool
		wantKind types.ErrorKind
	}{
		{"end_turn", false, 0},
		{"refusal", true, types.ErrorKindContentFilter},
		{"pause_turn", true, types.ErrorKindPermanent},
		{"model_context_window_exceeded", true, types.ErrorKindContextLength},
	} {
		t.Run(tc.stop, func(t *testing.T) {
			events := []sseEvent{{"message_start", evStart}, {"content_block_start", evTextStart},
				{"content_block_delta", evText("partial")}, {"content_block_stop", evBlockStop},
				{"message_delta", evMessageDelta(tc.stop, 5)}, {"message_stop", evStop}}
			a := NewAdapter("test", testModel, WithBaseURL(sseServer(t, false, events...).URL))
			r := run(t, a, nil)
			if !tc.wantErr {
				if len(r.errs) != 0 {
					t.Fatalf("errors = %v, want none", r.errs)
				}
				return
			}
			if len(r.errs) != 1 || types.KindOf(r.errs[0]) != tc.wantKind || types.IsTransient(r.errs[0]) {
				t.Fatalf("errors = %v, want one %v error", r.errs, tc.wantKind)
			}
			if tc.wantKind == types.ErrorKindContentFilter && !types.IsContentFilter(r.errs[0]) {
				t.Fatalf("IsContentFilter(%v) = false", r.errs[0])
			}
		})
	}
}
