package isolate_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/urmzd/saige/sandbox/isolate/egress"
	"github.com/urmzd/saige/sandbox/isolate/local"

	"github.com/urmzd/saige/flow/isolation"
	poolleaser "github.com/urmzd/saige/flow/isolation/isolate"
)

// gateLeaser is a Leaser over the local driver whose instances go out
// through a real gate, served on loopback.
func gateLeaser(t *testing.T, onDecision func(egress.Decision)) (*poolleaser.Leaser, string) {
	t.Helper()
	gate := egress.New(egress.Config{AllowNonPublic: true, OnDecision: onDecision}) // the upstreams are on loopback
	srv := httptest.NewServer(gate)
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	l, err := poolleaser.New(local.Driver{Dir: t.TempDir(), Log: io.Discard}, poolleaser.Config{
		Image:  "local",
		Egress: &poolleaser.Egress{Gate: gate, Address: addr},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close(context.Background()) })
	return l, addr
}

// proxyOf is the proxy URL a command in inst is given.
func proxyOf(t *testing.T, inst isolation.Instance, in isolation.Compartment) *url.URL {
	t.Helper()
	var out bytes.Buffer
	st, err := inst.Exec(context.Background(), isolation.Command{
		Argv: []string{"sh", "-c", `printf %s "$HTTPS_PROXY"`},
		Env:  map[string]string{"HTTPS_PROXY": "http://elsewhere:1"},
		In:   in,
	}, isolation.Stdio{Stdout: &out})
	if err != nil || st.Code != 0 {
		t.Fatalf("exec: %v, exit %d", err, st.Code)
	}
	u, err := url.Parse(out.String())
	if err != nil || u.Host == "" {
		t.Fatalf("the command's proxy = %q", out.String())
	}
	return u
}

func get(t *testing.T, proxy *url.URL, target string) int {
	t.Helper()
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxy)}}
	resp, err := c.Get(target)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// A lease reaches the hosts its request names and nothing else; its
// commands are given the gate whatever their own environment says; and
// releasing it ends its grant.
func TestALeaseReachesOnlyItsHostsThroughTheGate(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer up.Close()
	upURL, _ := url.Parse(up.URL)

	var mu sync.Mutex
	var decisions []egress.Decision
	l, addr := gateLeaser(t, func(d egress.Decision) {
		mu.Lock()
		decisions = append(decisions, d)
		mu.Unlock()
	})
	req := isolation.Request{Tenant: "acme", Run: "r1", Connectors: true, Egress: []string{upURL.Hostname()}}
	inst, err := l.Lease(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	proxy := proxyOf(t, inst, isolation.Workload)
	if proxy.Host != addr {
		t.Fatalf("the command was sent to %s, not the gate at %s", proxy.Host, addr)
	}
	if p := proxyOf(t, inst, isolation.Connectors); p.String() != proxy.String() {
		t.Fatalf("the connectors compartment got %s, the workload %s", p, proxy)
	}
	if got, ok := l.Granted(inst.ID()); !ok || got.Run != "r1" || got.Tenant != "acme" {
		t.Fatalf("Granted = %+v, %v", got, ok)
	}

	// The gate's default ports are 80 and 443, so a rule for the upstream's
	// host does not open its test port: the policy is hosts on the
	// standard ports, nothing more.
	if code := get(t, proxy, up.URL); code != http.StatusForbidden {
		t.Fatalf("a host on a port no rule names = %d, want 403", code)
	}
	if code := get(t, proxy, "http://denied.example/"); code != http.StatusForbidden {
		t.Fatalf("a host the request does not name = %d, want 403", code)
	}

	mu.Lock()
	seen := len(decisions)
	allDenied := true
	for _, d := range decisions {
		allDenied = allDenied && !d.Allowed && d.Grant == inst.ID()
	}
	mu.Unlock()
	if seen != 2 || !allDenied {
		t.Fatalf("decisions = %+v", decisions)
	}

	if err := inst.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := l.Granted(inst.ID()); ok {
		t.Fatal("the grant outlived its lease")
	}
	if code := get(t, proxy, "http://denied.example/"); code != http.StatusProxyAuthRequired {
		t.Fatalf("a released lease's token = %d, want 407", code)
	}
}

// The policy a lease gets is its hosts on the gate's default ports, and an
// empty request allows nothing.
func TestPolicy(t *testing.T) {
	p := poolleaser.Policy([]string{"API.github.com", " ", "*.pypi.org"})
	if len(p.Allow) != 2 || p.Allow[0].Host != "api.github.com" || p.Allow[1].Host != "*.pypi.org" || len(p.Allow[0].Ports) != 0 {
		t.Fatalf("policy = %+v", p)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if p := poolleaser.Policy(nil); len(p.Allow) != 0 {
		t.Fatalf("no hosts = %+v", p)
	}
}

// An allowed host is reached, and its decision says so.
func TestAnAllowedHostIsReached(t *testing.T) {
	// A gate on loopback that allows 127.0.0.1 on the test server's port,
	// granted by hand: the Leaser grants only default ports.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer up.Close()
	upURL, _ := url.Parse(up.URL)
	gate := egress.New(egress.Config{AllowNonPublic: true})
	ports := portsOf(t, upURL)
	l, err := poolleaser.New(local.Driver{Dir: t.TempDir(), Log: io.Discard}, poolleaser.Config{
		Image:  "local",
		Egress: &poolleaser.Egress{Gate: widen{gate, ports}, Address: "127.0.0.1:1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close(context.Background()) }()
	inst, err := l.Lease(context.Background(), isolation.Request{Run: "r", Egress: []string{upURL.Hostname()}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inst.Close(context.Background()) }()
	srv := httptest.NewServer(gate)
	defer srv.Close()
	proxy := proxyOf(t, inst, isolation.Workload)
	proxy.Host = strings.TrimPrefix(srv.URL, "http://")
	if code := get(t, proxy, up.URL); code != http.StatusOK {
		t.Fatalf("an allowed host = %d, want 200", code)
	}
}

// widen opens extra ports on every rule, so a test can reach a server on
// an ephemeral port.
type widen struct {
	g     *egress.Gate
	ports []int
}

func (w widen) Grant(id string, p egress.Policy) (string, error) {
	for i := range p.Allow {
		p.Allow[i].Ports = w.ports
	}
	return w.g.Grant(id, p)
}
func (w widen) Revoke(id string) { w.g.Revoke(id) }

func portsOf(t *testing.T, u *url.URL) []int {
	t.Helper()
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return []int{port}
}

// A lease whose grant fails is released, never handed out without it.
func TestALeaseWhoseGrantFailsIsNotHandedOut(t *testing.T) {
	l, err := poolleaser.New(local.Driver{Dir: t.TempDir(), Log: io.Discard}, poolleaser.Config{
		Image:  "local",
		Egress: &poolleaser.Egress{Gate: refusing{}, Address: "127.0.0.1:3128"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close(context.Background()) }()
	if _, err := l.Lease(context.Background(), isolation.Request{Run: "r"}); !errors.Is(err, errRefused) {
		t.Fatalf("lease = %v, want the grant's error", err)
	}
	if s := l.Stats(); s.Leased != 0 {
		t.Fatalf("a lease is still out: %+v", s)
	}
}

var errRefused = errors.New("refused")

type refusing struct{}

func (refusing) Grant(string, egress.Policy) (string, error) { return "", errRefused }
func (refusing) Revoke(string)                               {}

// Egress without a gate or an address is refused when the Leaser is made.
func TestEgressNeedsAGateAndAnAddress(t *testing.T) {
	for _, e := range []*poolleaser.Egress{{Address: "127.0.0.1:3128"}, {Gate: refusing{}}, {Gate: refusing{}, Address: "no-port"}} {
		if _, err := poolleaser.New(local.Driver{Dir: t.TempDir(), Log: io.Discard}, poolleaser.Config{Image: "local", Egress: e}); err == nil {
			t.Fatalf("egress %+v was accepted", e)
		}
	}
}
