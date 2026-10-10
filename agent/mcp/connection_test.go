package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/registry"
	"github.com/urmzd/saige/agent/types"
)

// flakyFront answers the first n requests matching a JSON-RPC method with a
// fixed status, then passes requests through to the real server.
type flakyFront struct {
	inner  http.Handler
	method string
	status int
	header http.Header
	fail   atomic.Int64
	seen   atomic.Int64
}

func (f *flakyFront) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		var msg struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &msg)
		if msg.Method == f.method {
			f.seen.Add(1)
			if f.fail.Add(-1) >= 0 {
				for k, v := range f.header {
					w.Header()[k] = v
				}
				w.WriteHeader(f.status)
				return
			}
		}
	}
	f.inner.ServeHTTP(w, r)
}

func TestRetryPolicyRespectsIdempotency(t *testing.T) {
	retryAfter := http.Header{"Retry-After": {"0"}}
	tests := []struct {
		name      string
		method    string
		status    int
		header    http.Header
		tool      string
		failures  int64
		trust     bool
		wantOK    bool
		wantSeen  int64
		connectRT bool
	}{
		{name: "429 on tools/call is retried", method: "tools/call", status: 429, header: retryAfter, tool: "t_wipe", failures: 2, wantOK: true, wantSeen: 3},
		{name: "503 with Retry-After is retried", method: "tools/call", status: 503, header: retryAfter, tool: "t_wipe", failures: 1, wantOK: true, wantSeen: 2},
		{name: "401 is not retried", method: "tools/call", status: 401, tool: "t_echo", failures: 1, wantOK: false, wantSeen: 1},
		{name: "502 on an unmarked tool is not retried", method: "tools/call", status: 502, tool: "t_wipe", failures: 1, wantOK: false, wantSeen: 1},
		{name: "502 on a trusted read-only tool is retried", method: "tools/call", status: 502, tool: "t_echo", failures: 1, trust: true, wantOK: true, wantSeen: 2},
		{name: "502 on an untrusted read-only hint is not retried", method: "tools/call", status: 502, tool: "t_echo", failures: 1, wantOK: false, wantSeen: 1},
		{name: "500 on a call is not retried", method: "tools/call", status: 500, tool: "t_echo", failures: 1, wantOK: false, wantSeen: 1},
		{name: "500 on the handshake is retried", method: "initialize", status: 500, tool: "t_echo", failures: 2, wantOK: true, wantSeen: 3, connectRT: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := newTestServer(t)
			front := &flakyFront{inner: *ts.handler.Load(), method: tt.method, status: tt.status, header: tt.header}
			srv := httptest.NewServer(front)
			t.Cleanup(srv.Close)

			var retries atomic.Int64
			policy := &RetryPolicy{MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond,
				OnRetry: func(RetryAttempt) { retries.Add(1) }}
			spec := Remote("t", srv.URL)
			spec.Retry = policy
			spec.ConnectRetry = policy
			spec.TrustHints = tt.trust

			c, err := Connect(context.Background(), spec)
			if err != nil {
				t.Fatalf("Connect: %v", err)
			}
			t.Cleanup(func() { _ = c.Close() })
			front.fail.Store(tt.failures)
			front.seen.Store(0)
			if tt.connectRT {
				// Reconnect so the handshake runs against the failing front.
				front.fail.Store(tt.failures)
				c2, err := Connect(context.Background(), spec)
				if err != nil {
					t.Fatalf("handshake not retried: %v", err)
				}
				_ = c2.Close()
				if got := front.seen.Load(); got != tt.wantSeen {
					t.Errorf("initialize requests = %d, want %d", got, tt.wantSeen)
				}
				return
			}

			res, err := toolNamed(t, c, tt.tool).ExecuteRich(context.Background(), map[string]any{"text": "x"})
			if err != nil {
				t.Fatal(err)
			}
			if res.IsError == tt.wantOK {
				t.Errorf("IsError=%v (%s), want ok=%v", res.IsError, res.Text(), tt.wantOK)
			}
			if got := front.seen.Load(); got != tt.wantSeen {
				t.Errorf("tools/call requests = %d, want %d", got, tt.wantSeen)
			}
			if got := retries.Load(); got != tt.wantSeen-1 {
				t.Errorf("OnRetry fired %d times, want %d", got, tt.wantSeen-1)
			}
		})
	}
}

func TestRetryTotalDelayCap(t *testing.T) {
	ts := newTestServer(t)
	front := &flakyFront{inner: *ts.handler.Load(), method: "tools/call", status: 429, header: http.Header{"Retry-After": {"60"}}}
	srv := httptest.NewServer(front)
	t.Cleanup(srv.Close)
	spec := Remote("t", srv.URL)
	spec.Retry = &RetryPolicy{MaxAttempts: 5, MaxTotalDelay: time.Second}
	c := connect(t, spec)
	front.fail.Store(1)

	start := time.Now()
	res, _ := toolNamed(t, c, "t_echo").ExecuteRich(context.Background(), nil)
	if !res.IsError || time.Since(start) > 2*time.Second {
		t.Errorf("a Retry-After beyond the total cap must fail fast: %v, %+v", time.Since(start), res)
	}
}

func TestTokenFuncSetsBearerPerRequest(t *testing.T) {
	ts := newTestServer(t)
	var mu sync.Mutex
	var seen []string
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		(*ts.handler.Load()).ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	spec := Remote("t", srv.URL)
	spec.TokenFunc = func(context.Context) (string, error) {
		return "tok-" + string(rune('0'+n.Add(1))), nil
	}
	c := connect(t, spec)
	if res, _ := toolNamed(t, c, "t_echo").ExecuteRich(context.Background(), map[string]any{"text": "x"}); res.IsError {
		t.Fatal(res.Text())
	}
	mu.Lock()
	got := append([]string(nil), seen...)
	mu.Unlock()
	if len(got) < 2 || got[0] != "Bearer tok-1" || got[0] == got[len(got)-1] {
		t.Errorf("Authorization headers = %v, want a fresh token per request", got)
	}

	failing := Remote("t", srv.URL)
	failing.TokenFunc = func(context.Context) (string, error) { return "", errors.New("vault sealed") }
	if _, err := Connect(context.Background(), failing); err == nil || !strings.Contains(err.Error(), "vault sealed") {
		t.Errorf("token failure: %v", err)
	}
}

func TestSpecValidationOfNewFields(t *testing.T) {
	tok := func(context.Context) (string, error) { return "", nil }
	tests := []struct {
		name string
		spec ServerSpec
		ok   bool
	}{
		{"token on remote", ServerSpec{Name: "a", URL: "https://x", TokenFunc: tok}, true},
		{"token on local", ServerSpec{Name: "a", Command: "x", TokenFunc: tok}, false},
		{"token and Authorization header", ServerSpec{Name: "a", URL: "https://x", TokenFunc: tok, Headers: map[string]string{"authorization": "Bearer y"}}, false},
		{"negative concurrency", ServerSpec{Name: "a", URL: "https://x", MaxConcurrent: -1}, false},
		{"negative retry", ServerSpec{Name: "a", URL: "https://x", Retry: &RetryPolicy{MaxAttempts: -1}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.spec.Validate(); (err == nil) != tt.ok {
				t.Errorf("Validate = %v, want ok=%v", err, tt.ok)
			}
		})
	}
}

func TestIdentity(t *testing.T) {
	base := ServerSpec{Name: "a", URL: "https://x", Headers: map[string]string{"X-Key": "1"}, AllowedTools: []string{"b", "a"}}
	same := ServerSpec{Name: "a", URL: "https://x", Headers: map[string]string{"X-Key": "1"}, AllowedTools: []string{"a", "b"}}
	if base.Identity() != same.Identity() {
		t.Error("allowlist order must not change the identity")
	}
	for name, mutate := range map[string]func(*ServerSpec){
		"header value": func(s *ServerSpec) { s.Headers = map[string]string{"X-Key": "2"} },
		"allowlist":    func(s *ServerSpec) { s.AllowedTools = []string{"a"} },
		"prefix":       func(s *ServerSpec) { s.ToolPrefix = "-" },
		"url":          func(s *ServerSpec) { s.URL = "https://y" },
	} {
		s := same
		mutate(&s)
		if s.Identity() == base.Identity() {
			t.Errorf("changing the %s must change the identity", name)
		}
	}
	if strings.Contains(base.Identity(), "1") && len(base.Identity()) != 64 {
		t.Error("identity must be a hash, never the header value")
	}
}

func TestPoolRefusesToShareAcrossPolicies(t *testing.T) {
	ts := newTestServer(t)
	deny := types.GateFunc(func(context.Context, types.ToolDef, map[string]any) types.GateDecision {
		return types.Deny("no")
	})
	tokenFor := func(who string) func(context.Context) (string, error) {
		return func(context.Context) (string, error) { return who, nil }
	}
	alice := tokenFor("alice")
	tests := []struct {
		name   string
		first  func(*ServerSpec)
		second func(*ServerSpec)
		share  bool
	}{
		{"identical specs share", func(*ServerSpec) {}, func(*ServerSpec) {}, true},
		{"same gate instance shares", func(s *ServerSpec) { s.Gate = deny }, func(s *ServerSpec) { s.Gate = deny }, true},
		{"gate only on the second", func(*ServerSpec) {}, func(s *ServerSpec) { s.Gate = deny }, false},
		{"same token func shares", func(s *ServerSpec) { s.TokenFunc = alice }, func(s *ServerSpec) { s.TokenFunc = alice }, true},
		{"token funcs for different principals", func(s *ServerSpec) { s.TokenFunc = alice }, func(s *ServerSpec) { s.TokenFunc = tokenFor("bob") }, false},
		{"different http client", func(*ServerSpec) {}, func(s *ServerSpec) { s.HTTPClient = &http.Client{} }, false},
		{"different idempotent tools", func(*ServerSpec) {}, func(s *ServerSpec) { s.IdempotentTools = []string{"wipe"} }, false},
		{"different limits", func(*ServerSpec) {}, func(s *ServerSpec) { s.MaxResultBytes = 1 }, false},
		{"equal retry policies share", func(s *ServerSpec) { s.Retry = DefaultRetryPolicy() }, func(s *ServerSpec) { s.Retry = DefaultRetryPolicy() }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := NewPool()
			t.Cleanup(func() { _ = pool.Close() })
			a, b := ts.spec(), ts.spec()
			tt.first(&a)
			tt.second(&b)
			if a.Identity() != b.Identity() {
				t.Fatal("test specs must share an identity")
			}
			first, err := pool.Add(context.Background(), a)
			if err != nil {
				t.Fatal(err)
			}
			second, err := pool.Add(context.Background(), b)
			if tt.share {
				if err != nil || second != first {
					t.Errorf("Add = %v, %v; want the pooled client", second, err)
				}
				return
			}
			if err == nil {
				t.Fatal("a spec with a different policy shared the pooled client")
			}
			if len(pool.Clients()) != 1 {
				t.Errorf("clients = %d, want 1", len(pool.Clients()))
			}
		})
	}
}

func TestCoerceScalarsToArrays(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{
		"tags":{"type":"array"},
		"maybe":{"type":["array","null"]},
		"either":{"type":["array","string"]},
		"name":{"type":"string"}}}`)
	tests := []struct {
		name string
		in   map[string]any
		want map[string]any
	}{
		{"scalar wrapped", map[string]any{"tags": "go"}, map[string]any{"tags": []any{"go"}}},
		{"array kept", map[string]any{"tags": []any{"a"}}, map[string]any{"tags": []any{"a"}}},
		{"nullable array", map[string]any{"maybe": 3.0}, map[string]any{"maybe": []any{3.0}}},
		{"null kept", map[string]any{"maybe": nil}, map[string]any{"maybe": nil}},
		{"union allowing the scalar", map[string]any{"either": "x"}, map[string]any{"either": "x"}},
		{"string property", map[string]any{"name": "x"}, map[string]any{"name": "x"}},
		{"undeclared", map[string]any{"other": "x"}, map[string]any{"other": "x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CoerceScalarsToArrays("t", schema, tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCheckAddress(t *testing.T) {
	tests := []struct {
		addr         string
		blockPrivate bool
		blocked      bool
	}{
		{"169.254.169.254:80", false, true},
		{"[fd00:ec2::254]:80", false, true},
		{"[fe80::1]:80", false, true},
		{"[::ffff:169.254.169.254]:80", false, true},
		{"127.0.0.1:80", false, false},
		{"127.0.0.1:80", true, true},
		{"10.1.2.3:443", true, true},
		{"100.64.0.1:443", true, true},
		{"[::1]:443", true, true},
		{"0.0.0.0:443", true, true},
		{"93.184.216.34:443", true, false},
	}
	for _, tt := range tests {
		err := checkAddress(tt.addr, tt.blockPrivate)
		if (err != nil) != tt.blocked {
			t.Errorf("checkAddress(%s, %v) = %v, want blocked=%v", tt.addr, tt.blockPrivate, err, tt.blocked)
		}
		if err != nil && !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("%s: error does not match ErrBlockedAddress", tt.addr)
		}
	}
}

func TestSafeHTTPClientBlocksAtDialTime(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	if _, err := SafeHTTPClient(true).Get(srv.URL); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("loopback with blockPrivate: %v", err)
	}
	resp, err := SafeHTTPClient(false).Get(srv.URL)
	if err != nil {
		t.Fatalf("loopback without blockPrivate: %v", err)
	}
	_ = resp.Body.Close()
}

func TestLoadConfig(t *testing.T) {
	env := map[string]string{"TOKEN": "s3cret", "HOME": "/home/u"}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }

	tests := []struct {
		name    string
		doc     string
		wantErr string
		check   func(t *testing.T, specs []ServerSpec)
	}{
		{name: "local and remote", doc: `{"mcpServers": {
			"fs": {"command": "mcp-fs", "args": ["${HOME}/src", "$LITERAL"], "env": {"LEVEL": "${LEVEL:-info}"}},
			"api": {"type": "http", "url": "https://mcp.example.com", "headers": {"Authorization": "Bearer ${TOKEN}"}}
		}}`, check: func(t *testing.T, specs []ServerSpec) {
			if len(specs) != 2 || specs[0].Name != "api" || specs[1].Name != "fs" {
				t.Fatalf("specs = %+v", specs)
			}
			api, fs := specs[0], specs[1]
			if api.Headers["Authorization"] != "Bearer s3cret" || api.HTTPClient == nil {
				t.Errorf("api = %+v", api)
			}
			if !reflect.DeepEqual(fs.Args, []string{"/home/u/src", "$LITERAL"}) {
				t.Errorf("args = %v", fs.Args)
			}
			if fs.Env[len(fs.Env)-1] != "LEVEL=info" || len(fs.Env) < 2 {
				t.Errorf("env must extend the process environment: %v", fs.Env[len(fs.Env)-1])
			}
		}},
		{name: "missing variable", doc: `{"mcpServers": {"api": {"url": "https://x/${NOPE}"}}}`, wantErr: "NOPE"},
		{name: "legacy sse", doc: `{"mcpServers": {"api": {"type": "sse", "url": "https://x"}}}`, wantErr: "SSE"},
		{name: "no transport", doc: `{"mcpServers": {"api": {}}}`, wantErr: "needs either"},
		{name: "bad json", doc: `{`, wantErr: "parse config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".mcp.json")
			if err := os.WriteFile(path, []byte(tt.doc), 0o600); err != nil {
				t.Fatal(err)
			}
			specs, err := LoadConfig(path, WithEnvLookup(lookup))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, specs)
		})
	}
}

func TestLoadConfigRecordsRevisions(t *testing.T) {
	reg := registry.New[ServerSpec]()
	doc := []byte(`{"mcpServers": {"api": {"url": "https://x"}}}`)
	for i := 0; i < 2; i++ {
		if _, err := ParseConfig(doc, WithConfigRegistry(reg)); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(reg.History("api")); got != 2 {
		t.Errorf("revisions = %d, want 2", got)
	}
	specs, _ := ParseConfig(doc, WithConfigHTTPClient(nil))
	if specs[0].HTTPClient != nil {
		t.Error("WithConfigHTTPClient(nil) must remove the default guard")
	}
}

func TestExpiredCallerDoesNotFailSharedHandshake(t *testing.T) {
	ts := newTestServer(t)
	pool := NewPool()
	t.Cleanup(func() { _ = pool.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := pool.Acquire(ctx, ts.spec()); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled caller: %v", err)
	}
	if _, err := pool.Acquire(context.Background(), ts.spec()); err != nil {
		t.Errorf("live caller: %v", err)
	}
}

var _ types.ToolGate = CapabilityGate(CapabilityPolicy{})

func TestAcquireReplacesAClientClosedOutsideThePool(t *testing.T) {
	ts := newTestServer(t)
	pool := NewPool()
	t.Cleanup(func() { _ = pool.Close() })
	first, err := pool.Acquire(context.Background(), ts.spec())
	if err != nil {
		t.Fatal(err)
	}
	_ = first.Close()
	second, err := pool.Acquire(context.Background(), ts.spec())
	if err != nil || second == first || len(pool.Clients()) != 1 {
		t.Errorf("second=%p first=%p clients=%d err=%v", second, first, len(pool.Clients()), err)
	}
}
