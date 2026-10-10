package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/cache/memcache"
	"github.com/urmzd/saige/agent/provider/cache"
	"github.com/urmzd/saige/agent/provider/internal/streamcheck"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

// lineServer writes each line, sleeping gap before every line after the
// first. With hangUp set it drops the connection after the last line.
func lineServer(t *testing.T, gap time.Duration, hangUp bool, lines ...string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for i, line := range lines {
			if i > 0 && gap > 0 {
				time.Sleep(gap)
			}
			_, _ = w.Write([]byte(line + "\n"))
			w.(http.Flusher).Flush()
		}
		if hangUp {
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close()
			}
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func line(c ChatChunk) string {
	b, _ := json.Marshal(c)
	return string(b)
}

func content(s string) string {
	return line(ChatChunk{Message: ChatMessage{Role: "assistant", Content: s}})
}

var doneLine = line(ChatChunk{Done: true, DoneReason: "stop", EvalCount: 3})

func runAdapter(t *testing.T, a *Adapter, schema *types.ParameterSchema) (string, []error) {
	t.Helper()
	ch, err := a.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("hi"))}, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	var errs []error
	var all []types.Delta
	for d := range ch {
		all = append(all, d)
		switch v := d.(type) {
		case types.PartDelta:
			text.WriteString(v.Text)
		case types.ErrorDelta:
			errs = append(errs, v.Error)
		}
	}
	streamcheck.RunPartConformance(t, all)
	return text.String(), errs
}

func TestStreamIntegrity(t *testing.T) {
	big := strings.Repeat("x", 100<<10)
	for _, tc := range []struct {
		name          string
		gap           time.Duration
		idle          time.Duration
		hangUp        bool
		lines         []string
		schema        bool
		wantText      string
		wantErr       string // substring; "" means no error
		wantTransient bool
		wantTruncated bool
	}{
		{name: "complete stream", lines: []string{content("ok"), doneLine}, wantText: "ok"},
		{name: "100KB line decodes", lines: []string{content(big), doneLine}, wantText: big},
		{name: "closed without done before content", hangUp: true, lines: []string{line(ChatChunk{Message: ChatMessage{Role: "assistant"}})}, wantErr: "unexpected EOF", wantTransient: true},
		{name: "closed without done after content", hangUp: true, lines: []string{content("par")}, wantText: "par", wantErr: "unexpected EOF"},
		{name: "clean EOF without done", lines: []string{content("par")}, wantText: "par", wantErr: "unexpected EOF"},
		{name: "server error line", lines: []string{content("par"), `{"error":"model crashed"}`}, wantText: "par", wantErr: "model crashed"},
		{name: "malformed line", lines: []string{`{"message":`}, wantErr: "malformed line"},
		{name: "slow but steady stream is not cut off", gap: 30 * time.Millisecond, idle: 150 * time.Millisecond,
			lines: []string{content("a"), content("b"), content("c"), content("d"), content("e"), content("f"), doneLine}, wantText: "abcdef"},
		{name: "stalled stream times out", gap: 400 * time.Millisecond, idle: 100 * time.Millisecond,
			lines: []string{line(ChatChunk{Message: ChatMessage{Role: "assistant"}}), doneLine}, wantErr: "idle", wantTransient: true},
		{name: "structured output cut at length", schema: true,
			lines: []string{content(`{"a":"`), line(ChatChunk{Done: true, DoneReason: "length"})}, wantText: `{"a":"`, wantErr: "truncated", wantTruncated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := lineServer(t, tc.gap, tc.hangUp, tc.lines...)
			client := NewClient(server.URL, "test-model", "")
			if tc.idle > 0 {
				client.StreamIdleTimeout = tc.idle
			}
			var schema *types.ParameterSchema
			if tc.schema {
				schema = &types.ParameterSchema{Type: "object"}
			}
			text, errs := runAdapter(t, NewAdapter(client), schema)
			if text != tc.wantText {
				t.Fatalf("text length %d, want %d", len(text), len(tc.wantText))
			}
			if tc.wantErr == "" {
				if len(errs) != 0 {
					t.Fatalf("unexpected errors %v", errs)
				}
				return
			}
			if len(errs) != 1 || !strings.Contains(errs[0].Error(), tc.wantErr) {
				t.Fatalf("errors = %v, want one containing %q", errs, tc.wantErr)
			}
			if types.IsTransient(errs[0]) != tc.wantTransient || types.IsTruncated(errs[0]) != tc.wantTruncated {
				t.Fatalf("transient = %v truncated = %v: %v", types.IsTransient(errs[0]), types.IsTruncated(errs[0]), errs[0])
			}
		})
	}
}

func TestTruncatedStreamNotCached(t *testing.T) {
	server := lineServer(t, 0, true, content("partial"))
	p := must.Get(cache.New(NewAdapter(NewClient(server.URL, "test-model", "")), cache.Config{Cache: memcache.New[cache.CachedResponse]()}))
	msgs := []types.Message{types.UserMsg(types.Text("hi"))}
	for i := range 2 {
		ch, err := p.Stream(context.Background(), types.Request{Messages: msgs})
		if err != nil {
			t.Fatal(err)
		}
		sawErr, hit := false, false
		for d := range ch {
			switch v := d.(type) {
			case types.ErrorDelta:
				sawErr = true
			case types.UsageDelta:
				hit = hit || v.CacheHit
			}
		}
		if hit || !sawErr {
			t.Fatalf("call %d: hit = %v error = %v; a truncated stream was cached", i, hit, sawErr)
		}
	}
}

func TestRequestErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		header    map[string]string
		body      string
		wantKind  types.ErrorKind
		wantAfter time.Duration
	}{
		{"rate limited", 429, map[string]string{"Retry-After": "7"}, `{"error":"busy"}`, types.ErrorKindRateLimit, 7 * time.Second},
		{"model missing", 404, nil, `{"error":"model 'x' not found, try pulling it first"}`, types.ErrorKindInvalidRequest, 0},
		{"server error", 500, nil, `{"error":"runner crashed"}`, types.ErrorKindUnavailable, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			_, err := NewAdapter(NewClient(server.URL, "test-model", "")).Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("hi"))}})
			var pe *types.ProviderError
			if !errors.As(err, &pe) || pe.Kind != tc.wantKind || pe.RetryAfter != tc.wantAfter || pe.Code != tc.status {
				t.Fatalf("err = %#v", err)
			}
		})
	}

	// A refused connection is a transport failure, so retries may act on it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	_, err = NewAdapter(NewClient("http://"+addr, "test-model", "")).Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("hi"))}})
	if !types.IsTransient(err) {
		t.Fatalf("refused connection: err = %v, want transient", err)
	}
}

func TestErrorBodyIsBounded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(strings.Repeat("e", 1<<20)))
	}))
	defer server.Close()
	_, err := NewClient(server.URL, "m", "").ChatStream(context.Background(), nil, nil)
	var se *StatusError
	if !errors.As(err, &se) || len(se.Body) > maxErrorBodyBytes {
		t.Fatalf("err = %T, body length unbounded", err)
	}
}

func TestDefaultClientHasNoBodyTimeout(t *testing.T) {
	c := NewClient("http://localhost", "m", "")
	if c.HTTP.Timeout != 0 {
		t.Fatalf("http.Client.Timeout = %v; it would cut off long streams", c.HTTP.Timeout)
	}
	tr, ok := c.HTTP.Transport.(*http.Transport)
	if !ok || tr.ResponseHeaderTimeout != DefaultResponseHeaderTimeout {
		t.Fatal("response header timeout not set")
	}
}
