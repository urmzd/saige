package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	agenttypes "github.com/urmzd/saige/agent/types"
)

const testToken = "test-token-0123456789abcdef"

// bearer adds a bearer token to every request.
type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

// statusTransport records the HTTP statuses the client saw.
type statusTransport struct {
	base     http.RoundTripper
	mu       sync.Mutex
	statuses []int
}

func (s *statusTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := s.base.RoundTrip(r)
	if err == nil {
		s.mu.Lock()
		s.statuses = append(s.statuses, resp.StatusCode)
		s.mu.Unlock()
	}
	return resp, err
}

func (s *statusTransport) saw(code int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.statuses {
		if c == code {
			return true
		}
	}
	return false
}

// httpServer serves the bridged tools over streamable HTTP on a test server.
func httpServer(t *testing.T, b bridge, cfg httpConfig, tools ...agenttypes.Tool) *httptest.Server {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "saige-mcp", Version: "test"}, nil)
	for _, tool := range tools {
		b.register(server, tool)
	}
	return startHTTP(t, server, cfg)
}

func startHTTP(t *testing.T, server *mcp.Server, cfg httpConfig) *httptest.Server {
	t.Helper()
	if cfg.path == "" {
		cfg.path = defaultPath
	}
	if cfg.tokens == nil {
		cfg.tokens = []string{testToken}
	}
	if cfg.rate == 0 {
		cfg.rate, cfg.burst = 1000, 1000
	}
	ts := httptest.NewServer(httpHandler(server, cfg))
	t.Cleanup(ts.Close)
	return ts
}

// httpClient connects a go-sdk client to ts with token.
func httpClient(t *testing.T, ts *httptest.Server, token string, elicit func(*mcp.ElicitRequest) (*mcp.ElicitResult, error)) (*mcp.ClientSession, *statusTransport, error) {
	t.Helper()
	st := &statusTransport{base: http.DefaultTransport}
	var rt http.RoundTripper = st
	if token != "" {
		rt = bearer{token: token, base: st}
	}
	opts := &mcp.ClientOptions{}
	if elicit != nil {
		opts.ElicitationHandler = func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) { return elicit(req) }
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, opts)
	cs, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: ts.URL + defaultPath, HTTPClient: &http.Client{Transport: rt}, MaxRetries: -1,
	}, nil)
	if err == nil {
		t.Cleanup(func() { _ = cs.Close() })
	}
	return cs, st, err
}

func echoTool() agenttypes.Tool {
	return &agenttypes.ToolFunc{
		Def: agenttypes.ToolDef{Name: "echo", Capability: agenttypes.ToolCapabilityRead,
			Parameters: agenttypes.ParameterSchema{Type: "object", Properties: map[string]agenttypes.PropertyDef{"text": {Type: "string"}}}},
		Fn: func(_ context.Context, args map[string]any) (string, error) {
			return "echo: " + args["text"].(string), nil
		},
	}
}

func TestHTTPRoundTrip(t *testing.T) {
	ts := httpServer(t, bridge{approval: approvalElicit}, httpConfig{}, echoTool())
	cs, _, err := httpClient(t, ts, testToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	list, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 1 || list.Tools[0].Name != "echo" {
		t.Fatalf("tools = %+v", list.Tools)
	}
	if got := resultText(call(t, cs, "echo")); got != "echo: x" {
		t.Fatalf("result = %q", got)
	}
}

func TestHTTPRejectsMissingAndWrongTokens(t *testing.T) {
	ts := httpServer(t, bridge{approval: approvalElicit}, httpConfig{}, echoTool())
	for name, token := range map[string]string{"missing": "", "wrong": "not-the-token-0123456789"} {
		t.Run(name, func(t *testing.T) {
			_, st, err := httpClient(t, ts, token, nil)
			if err == nil {
				t.Fatal("connected without a valid token")
			}
			if !st.saw(http.StatusUnauthorized) {
				t.Fatalf("statuses = %v, want a 401", st.statuses)
			}
		})
	}
	// A plain request gets 401 with a bearer challenge.
	resp, err := http.Post(ts.URL+defaultPath, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestHTTPRateLimitsPerToken(t *testing.T) {
	other := "second-token-0123456789abcdef"
	now := time.Unix(0, 0)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	// The handshake is two requests (initialize, initialized); the
	// standalone event stream is a third.
	ts := httpServer(t, bridge{approval: approvalElicit},
		httpConfig{tokens: []string{testToken, other}, rate: 1, burst: 4, now: clock}, echoTool())

	cs, st, err := httpClient(t, ts, testToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	var limited bool
	for range 5 {
		if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": "x"}}); err != nil {
			limited = true
			break
		}
	}
	if !limited || !st.saw(http.StatusTooManyRequests) {
		t.Fatalf("no 429 after the burst; statuses = %v", st.statuses)
	}

	// Another token has its own bucket.
	cs2, _, err := httpClient(t, ts, other, nil)
	if err != nil {
		t.Fatalf("second token was limited too: %v", err)
	}
	if got := resultText(call(t, cs2, "echo")); got != "echo: x" {
		t.Fatalf("result = %q", got)
	}

	// The bucket refills with time.
	mu.Lock()
	now = now.Add(10 * time.Second)
	mu.Unlock()
	if got := resultText(call(t, cs, "echo")); got != "echo: x" {
		t.Fatalf("result after refill = %q", got)
	}
}

func TestRateLimiterRetryAfter(t *testing.T) {
	now := time.Unix(0, 0)
	l := newRateLimiter(2, 1, func() time.Time { return now })
	if ok, _ := l.allow("a"); !ok {
		t.Fatal("first request refused")
	}
	ok, wait := l.allow("a")
	if ok || wait != 500*time.Millisecond {
		t.Fatalf("allow = %v, wait %v; want refused for 500ms", ok, wait)
	}
	now = now.Add(500 * time.Millisecond)
	if ok, _ := l.allow("a"); !ok {
		t.Fatal("refused after the refill")
	}
}

func TestHTTPEnforcesMarkersThroughElicitation(t *testing.T) {
	for _, tt := range []struct {
		name    string
		mode    approvalMode
		approve bool
		wantRun bool
	}{
		{"elicit approved", approvalElicit, true, true},
		{"elicit refused", approvalElicit, false, false},
		{"deny", approvalDeny, true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var runs atomic.Int64
			var asked atomic.Bool
			ts := httpServer(t, bridge{approval: tt.mode}, httpConfig{}, storeTool(&runs))
			cs, _, err := httpClient(t, ts, testToken, func(*mcp.ElicitRequest) (*mcp.ElicitResult, error) {
				asked.Store(true)
				return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"approve": tt.approve}}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			res := call(t, cs, "store")
			if ran := runs.Load() == 1; ran != tt.wantRun {
				t.Fatalf("tool ran=%v, want %v (%s)", ran, tt.wantRun, resultText(res))
			}
			if res.IsError == tt.wantRun {
				t.Errorf("IsError=%v", res.IsError)
			}
			if tt.mode == approvalElicit && !asked.Load() {
				t.Error("the client was not asked")
			}
		})
	}
}

func TestNewHTTPConfig(t *testing.T) {
	env := func(v string) func(string) string {
		return func(string) string { return v }
	}
	file := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(file, []byte("# comment\n\n"+testToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ok := httpFlags{addr: defaultAddr, path: defaultPath, tokenEnv: defaultTokenEnv, rate: 1, burst: 1}
	tests := []struct {
		name    string
		mutate  func(*httpFlags)
		env     string
		wantErr string
		tokens  int
	}{
		{"loopback with env tokens", nil, testToken + ", second-token-0123456789", "", 2},
		{"token file", func(f *httpFlags) { f.tokenFile = file }, "", "", 1},
		{"localhost name", func(f *httpFlags) { f.addr = "localhost:9000" }, testToken, "", 1},
		{"ipv6 loopback", func(f *httpFlags) { f.addr = "[::1]:9000" }, testToken, "", 1},
		{"no token", nil, "", "needs a bearer token", 0},
		{"short token", nil, "short", "shorter than", 0},
		{"all interfaces without TLS", func(f *httpFlags) { f.addr = ":8765" }, testToken, "refusing plain HTTP", 0},
		{"public address without TLS", func(f *httpFlags) { f.addr = "10.0.0.5:8765" }, testToken, "refusing plain HTTP", 0},
		{"public address behind a proxy", func(f *httpFlags) { f.addr = ":8765"; f.allowInsecure = true }, testToken, "", 1},
		{"public address with TLS", func(f *httpFlags) { f.addr = ":8765"; f.tlsCert, f.tlsKey = "c.pem", "k.pem" }, testToken, "", 1},
		{"cert without key", func(f *httpFlags) { f.tlsCert = "c.pem" }, testToken, "go together", 0},
		{"bad path", func(f *httpFlags) { f.path = "mcp" }, testToken, "must start with /", 0},
		{"zero rate", func(f *httpFlags) { f.rate = 0 }, testToken, "must be positive", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := ok
			if tt.mutate != nil {
				tt.mutate(&f)
			}
			cfg, err := newHTTPConfig(f, env(tt.env))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(cfg.tokens) != tt.tokens {
				t.Fatalf("tokens = %d, want %d", len(cfg.tokens), tt.tokens)
			}
		})
	}
}

func TestVerifierNeverEchoesTheToken(t *testing.T) {
	v := verifier([]string{testToken})
	info, err := v(context.Background(), testToken, nil)
	if err != nil || info.UserID == "" || strings.Contains(info.UserID, testToken) {
		t.Fatalf("info = %+v, err %v", info, err)
	}
	if _, err := v(context.Background(), testToken+"x", nil); err == nil {
		t.Fatal("a different token was accepted")
	} else if !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("err = %v", err)
	}
}
