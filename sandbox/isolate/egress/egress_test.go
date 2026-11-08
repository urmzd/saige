package egress

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const placeholder = "PLACEHOLDER-gh"

// upstream records every request it receives and echoes its headers.
type upstream struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []*http.Request
}

func newUpstream(t *testing.T, tlsServer bool) *upstream {
	u := &upstream{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.reqs = append(u.reqs, r.Clone(context.Background()))
		u.mu.Unlock()
		for _, k := range []string{"Authorization", "X-Other", "Proxy-Authorization"} {
			w.Header()[k+"-Seen"] = r.Header.Values(k)
		}
		_, _ = io.WriteString(w, "hello from "+r.Host+r.URL.Path)
	})
	if tlsServer {
		u.Server = httptest.NewTLSServer(h)
	} else {
		u.Server = httptest.NewServer(h)
	}
	t.Cleanup(u.Close)
	return u
}

func (u *upstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.reqs)
}

func (u *upstream) port(t *testing.T) int {
	_, p, _ := net.SplitHostPort(u.Listener.Addr().String())
	n, err := strconv.Atoi(p)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// fixture is a gate served on a test listener, with its decisions kept.
type fixture struct {
	gate      *Gate
	srv       *httptest.Server
	mu        sync.Mutex
	decisions []Decision
}

func newFixture(t *testing.T, cfg Config) *fixture {
	f := &fixture{}
	cfg.OnDecision = func(d Decision) {
		f.mu.Lock()
		f.decisions = append(f.decisions, d)
		f.mu.Unlock()
	}
	if cfg.Resolve == nil {
		cfg.Resolve = func(_ context.Context, c Credential) (string, error) {
			return "real-" + c.Name + "-for-" + c.Grant, nil
		}
	}
	f.gate = New(cfg)
	f.srv = httptest.NewServer(f.gate)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fixture) last() Decision {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.decisions) == 0 {
		return Decision{}
	}
	return f.decisions[len(f.decisions)-1]
}

// client sends requests through the gate with token; extra TLS roots come
// from roots, when set.
func (f *fixture) client(token string, roots *httptest.Server) *http.Client {
	proxy, _ := url.Parse(ProxyURL(f.srv.Listener.Addr().String(), token))
	tr := &http.Transport{Proxy: http.ProxyURL(proxy)}
	if roots != nil {
		tr.TLSClientConfig = roots.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	}
	return &http.Client{Transport: tr}
}

func get(t *testing.T, c *http.Client, rawURL string, header map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func ghRule(host string, port int) Rule {
	return Rule{Host: host, Ports: []int{port}, Inject: []Injection{{Header: "Authorization", Placeholder: placeholder, Credential: "gh"}}}
}

func TestPlainHTTPInjectsForAnAllowedHost(t *testing.T) {
	up := newUpstream(t, false)
	f := newFixture(t, Config{AllowNonPublic: true})
	token, err := f.gate.Grant("run-1", Policy{Allow: []Rule{ghRule("127.0.0.1", up.port(t))}})
	if err != nil {
		t.Fatal(err)
	}
	resp := get(t, f.client(token, nil), up.URL+"/repos", map[string]string{
		"Authorization": "Bearer " + placeholder,
		"X-Other":       placeholder, // not configured: left as it is
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Authorization-Seen"); got != "Bearer real-gh-for-run-1" {
		t.Fatalf("upstream saw Authorization %q", got)
	}
	if got := resp.Header.Get("X-Other-Seen"); got != placeholder {
		t.Fatalf("upstream saw X-Other %q", got)
	}
	if got := resp.Header.Values("Proxy-Authorization-Seen"); len(got) != 0 {
		t.Fatalf("the grant token leaked upstream: %q", got)
	}
	if d := f.last(); !d.Allowed || d.Grant != "run-1" || len(d.Injected) != 1 || d.Injected[0] != "Authorization" {
		t.Fatalf("decision %+v", d)
	}
}

// The heart of it: a request for a host the grant does not allow is refused
// before anything is dialed, so its placeholder never leaves the gate.
func TestPlaceholderNeverLeavesForADeniedHost(t *testing.T) {
	allowed, denied := newUpstream(t, false), newUpstream(t, true)
	f := newFixture(t, Config{AllowNonPublic: true})
	token, err := f.gate.Grant("run-1", Policy{Allow: []Rule{ghRule("127.0.0.1", allowed.port(t))}})
	if err != nil {
		t.Fatal(err)
	}
	// The denied server listens on the same address, another port.
	plain := "http://127.0.0.1:" + strconv.Itoa(denied.port(t)) + "/x"
	if resp := get(t, f.client(token, nil), plain, map[string]string{"Authorization": "Bearer " + placeholder}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("plain HTTP to a denied port: status %d", resp.StatusCode)
	}
	if d := f.last(); d.Allowed || d.Reason != "host not allowed" {
		t.Fatalf("decision %+v", d)
	}
	_, err = f.client(token, denied.Server).Get(denied.URL)
	if err == nil {
		t.Fatal("CONNECT to a denied port succeeded")
	}
	// Another name for the same address is another host.
	_, port, _ := net.SplitHostPort(allowed.Listener.Addr().String())
	if resp := get(t, f.client(token, nil), "http://localhost:"+port+"/", map[string]string{"Authorization": placeholder}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("an unlisted name for an allowed address: status %d", resp.StatusCode)
	}
	if allowed.count() != 0 || denied.count() != 0 {
		t.Fatalf("a denied request reached an upstream: %d, %d", allowed.count(), denied.count())
	}
}

func TestTokens(t *testing.T) {
	up := newUpstream(t, false)
	f := newFixture(t, Config{AllowNonPublic: true})
	token, err := f.gate.Grant("run-1", Policy{Allow: []Rule{{Host: "127.0.0.1", Ports: []int{up.port(t)}}}})
	if err != nil {
		t.Fatal(err)
	}
	for name, tok := range map[string]string{"no token": "", "wrong token": "nope"} {
		c := f.client(tok, nil)
		if tok == "" {
			c = &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: f.srv.Listener.Addr().String()})}}
		}
		if resp := get(t, c, up.URL, nil); resp.StatusCode != http.StatusProxyAuthRequired {
			t.Errorf("%s: status %d", name, resp.StatusCode)
		}
	}
	if resp := get(t, f.client(token, nil), up.URL, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("valid token: status %d", resp.StatusCode)
	}
	if _, err := f.gate.Grant("run-1", Policy{}); !errors.Is(err, ErrDuplicateGrant) {
		t.Fatalf("duplicate grant: %v", err)
	}
	f.gate.Revoke("run-1")
	if resp := get(t, f.client(token, nil), up.URL, nil); resp.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf("revoked token: status %d", resp.StatusCode)
	}
	if up.count() != 1 {
		t.Fatalf("upstream saw %d requests, want 1", up.count())
	}
	// One grant's token never reaches another grant's hosts.
	other, err := f.gate.Grant("run-2", Policy{})
	if err != nil {
		t.Fatal(err)
	}
	if resp := get(t, f.client(other, nil), up.URL, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("empty policy: status %d", resp.StatusCode)
	}
}

func TestConnectTunnelsWithoutInjecting(t *testing.T) {
	up := newUpstream(t, true)
	f := newFixture(t, Config{AllowNonPublic: true})
	token, err := f.gate.Grant("run-1", Policy{Allow: []Rule{ghRule("127.0.0.1", up.port(t))}})
	if err != nil {
		t.Fatal(err)
	}
	resp := get(t, f.client(token, up.Server), up.URL+"/secure", map[string]string{"Authorization": "Bearer " + placeholder})
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "/secure") {
		t.Fatalf("status %d, body %q", resp.StatusCode, body)
	}
	// Inside TLS the gate sees nothing, so the placeholder arrives as is.
	if got := resp.Header.Get("Authorization-Seen"); got != "Bearer "+placeholder {
		t.Fatalf("upstream saw %q through a tunnel", got)
	}
	if d := f.last(); !d.Allowed || d.Method != http.MethodConnect || len(d.Injected) != 0 {
		t.Fatalf("decision %+v", d)
	}
}

// A gate-terminated route: the sandbox speaks plain HTTP to a name, the
// gate speaks TLS to the real host and injects the credential.
func TestRouteTerminatesTLSAndInjects(t *testing.T) {
	up := newUpstream(t, true)
	f := newFixture(t, Config{
		AllowNonPublic:  true,
		TLSClientConfig: up.Client().Transport.(*http.Transport).TLSClientConfig,
	})
	rule := ghRule("github.route", 80)
	rule.Upstream = up.URL
	token, err := f.gate.Grant("run-1", Policy{Allow: []Rule{rule}})
	if err != nil {
		t.Fatal(err)
	}
	resp := get(t, f.client(token, nil), "http://github.route/user?x=1", map[string]string{"Authorization": "token " + placeholder})
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.HasSuffix(string(body), "/user") {
		t.Fatalf("status %d, body %q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Authorization-Seen"); got != "token real-gh-for-run-1" {
		t.Fatalf("upstream saw %q", got)
	}
	up.mu.Lock()
	host := up.reqs[0].Host
	up.mu.Unlock()
	if host != strings.TrimPrefix(up.URL, "https://") {
		t.Fatalf("upstream saw Host %q", host)
	}
	// CONNECT to a route is refused: the gate could not inject into it.
	if _, err := f.client(token, up.Server).Get("https://github.route/"); err == nil {
		t.Fatal("CONNECT to a route succeeded")
	}
}

func TestResolverFailureSendsNothing(t *testing.T) {
	up := newUpstream(t, false)
	f := newFixture(t, Config{AllowNonPublic: true, Resolve: func(context.Context, Credential) (string, error) {
		return "", errors.New("vault down")
	}})
	token, err := f.gate.Grant("run-1", Policy{Allow: []Rule{ghRule("127.0.0.1", up.port(t))}})
	if err != nil {
		t.Fatal(err)
	}
	if resp := get(t, f.client(token, nil), up.URL, map[string]string{"Authorization": placeholder}); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if up.count() != 0 {
		t.Fatal("a request without its credential was forwarded")
	}
}

func TestNonPublicAddressesAreRefusedByDefault(t *testing.T) {
	up := newUpstream(t, false)
	f := newFixture(t, Config{})
	token, err := f.gate.Grant("run-1", Policy{Allow: []Rule{{Host: "127.0.0.1", Ports: []int{up.port(t)}}, {Host: "localhost", Ports: []int{up.port(t)}}}})
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(up.Listener.Addr().String())
	for _, target := range []string{up.URL, "http://localhost:" + port} {
		if resp := get(t, f.client(token, nil), target, nil); resp.StatusCode != http.StatusBadGateway {
			t.Errorf("%s: status %d", target, resp.StatusCode)
		}
	}
	if up.count() != 0 {
		t.Fatal("the gate dialed a loopback address")
	}
	for ip, want := range map[string]bool{
		"8.8.8.8": true, "2606:4700::1111": true,
		"10.0.0.1": false, "172.16.0.1": false, "192.168.1.1": false, "169.254.169.254": false,
		"100.64.0.1": false, "0.0.0.0": false, "::1": false, "fe80::1": false, "fd00::1": false,
	} {
		if got := public(net.ParseIP(ip)); got != want {
			t.Errorf("public(%s) = %v", ip, got)
		}
	}
}

func TestBadRequests(t *testing.T) {
	f := newFixture(t, Config{AllowNonPublic: true})
	token, err := f.gate.Grant("run-1", Policy{Allow: []Rule{{Host: "example.com"}}})
	if err != nil {
		t.Fatal(err)
	}
	// An origin-form request is not a proxy request.
	req, _ := http.NewRequest(http.MethodGet, f.srv.URL+"/", nil)
	req.Header.Set("Proxy-Authorization", "Basic "+basic(token))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("origin-form: status %d", resp.StatusCode)
	}
}

func basic(token string) string {
	u := url.UserPassword(ProxyUser, token)
	r := &http.Request{Header: http.Header{}}
	p, _ := u.Password()
	r.SetBasicAuth(u.Username(), p)
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Basic ")
}

func TestMatch(t *testing.T) {
	p := clonePolicy(Policy{Allow: []Rule{{Host: "*.Example.com"}, {Host: "api.test", Ports: []int{8443}}}})
	for _, c := range []struct {
		host string
		port int
		want bool
	}{
		{"a.example.com", 443, true},
		{"A.B.EXAMPLE.COM", 80, true},
		{"example.com", 443, false},
		{"badexample.com", 443, false},
		{"a.example.com", 22, false},
		{"api.test", 8443, true},
		{"api.test", 443, false},
	} {
		if _, got := p.match(c.host, c.port); got != c.want {
			t.Errorf("match(%s, %d) = %v", c.host, c.port, got)
		}
	}
}

func TestPolicyValidate(t *testing.T) {
	inj := []Injection{{Header: "Authorization", Placeholder: "p", Credential: "c"}}
	valid := []Policy{
		{},
		{Allow: []Rule{{Host: "a.com"}, {Host: "*.a.com", Ports: []int{8080}}, {Host: "::1"}}},
		{Allow: []Rule{{Host: "gh.route", Upstream: "https://api.github.com", Inject: inj}}},
	}
	for _, p := range valid {
		if err := p.Validate(); err != nil {
			t.Errorf("%+v: %v", p, err)
		}
	}
	invalid := map[string]Rule{
		"empty host":        {},
		"bare wildcard":     {Host: "*"},
		"inner wildcard":    {Host: "a.*.com"},
		"host with port":    {Host: "a.com:443"},
		"bad port":          {Host: "a.com", Ports: []int{0}},
		"http upstream":     {Host: "r", Upstream: "http://a.com"},
		"upstream path":     {Host: "r", Upstream: "https://a.com/x"},
		"upstream userinfo": {Host: "r", Upstream: "https://u:p@a.com"},
		"wildcard route":    {Host: "*.r", Upstream: "https://a.com"},
		"no placeholder":    {Host: "a.com", Inject: []Injection{{Header: "Authorization", Credential: "c"}}},
		"no credential":     {Host: "a.com", Inject: []Injection{{Header: "Authorization", Placeholder: "p"}}},
		"host header":       {Host: "a.com", Inject: []Injection{{Header: "Host", Placeholder: "p", Credential: "c"}}},
		"proxy header":      {Host: "a.com", Inject: []Injection{{Header: "Proxy-Authorization", Placeholder: "p", Credential: "c"}}},
	}
	for name, r := range invalid {
		if err := (Policy{Allow: []Rule{r}}).Validate(); !errors.Is(err, ErrInvalidPolicy) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestProxyEnv(t *testing.T) {
	env := ProxyEnv("gate:3128", "tok")
	if env["HTTPS_PROXY"] != "http://playscape:tok@gate:3128" || env["http_proxy"] != env["HTTPS_PROXY"] {
		t.Fatalf("env %v", env)
	}
	if v, ok := env["NO_PROXY"]; !ok || v != "" {
		t.Fatal("NO_PROXY must be set empty, so nothing bypasses the gate")
	}
}
