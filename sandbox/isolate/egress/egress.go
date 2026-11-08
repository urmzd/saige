// Package egress is the egress gate: an HTTP forward proxy that is an
// instance's only way out. Each lease gets a grant: a policy (the hosts it
// may reach, default deny) and a token its commands present as proxy
// credentials. For an allowed host the gate can inject a credential: the
// sandbox holds only a placeholder, and the gate swaps in the real value,
// fetched from the caller's Resolver, on the way out. A placeholder that
// leaks is worthless anywhere else.
//
// What the gate can see decides what it can do:
//
//   - Plain HTTP through the proxy (http://host/...): the gate sees the
//     whole request, so it can allow or deny it and inject credentials.
//   - HTTPS through the proxy (CONNECT host:443): the gate sees only the
//     host and port, then relays encrypted bytes. It allows or denies the
//     tunnel, and cannot inject anything: that would take a man in the
//     middle, which this gate does not do. A placeholder sent inside the
//     tunnel reaches the allowed host unchanged.
//   - A gate-terminated route: a rule with an Upstream. The sandbox calls
//     http://<rule host>/... through the proxy, in plain HTTP, and the gate
//     forwards it over TLS to the Upstream, injecting credentials. This is
//     how a sandbox reaches an HTTPS API with a credential it never holds.
//
// Requests for a host no rule allows are refused with 403 before anything
// is dialed, so no part of them, a placeholder included, leaves the gate.
// By default the gate also refuses to dial loopback, private, link-local
// and other non-public addresses, whatever name led to them.
//
// Gate is an http.Handler. The program embedding it serves it on a
// listener the sandboxes can reach; the package itself starts nothing.
//
// Orthogonality boundary: egress decides and forwards; it does not know
// where credentials live (Resolver), which run a grant belongs to (the
// caller names it), or how sandboxes reach it (the isolate driver).
package egress

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// schemeHTTP is the only scheme the gate forwards outside a CONNECT tunnel.
const schemeHTTP = "http"

var (
	// ErrInvalidPolicy is returned by Grant for a malformed Policy.
	ErrInvalidPolicy = errors.New("egress: invalid policy")
	// ErrDuplicateGrant is returned by Grant for an ID already granted.
	ErrDuplicateGrant = errors.New("egress: grant already exists")
	// ErrDenied is the reason for a refused request.
	ErrDenied = errors.New("egress: denied")
	// ErrNonPublicAddress is returned when a dial would reach a
	// loopback, private or otherwise non-public address.
	ErrNonPublicAddress = errors.New("egress: non-public address")
)

// DefaultPorts are the ports a rule with no Ports allows.
var DefaultPorts = []int{80, 443}

// Policy is what one grant may reach. The zero value reaches nothing.
type Policy struct {
	Allow []Rule
}

// Rule allows one host, or one family of hosts.
type Rule struct {
	// Host is the host name the sandbox asks for: an exact name
	// ("api.example.com") or a wildcard for its subdomains
	// ("*.example.com", which does not match "example.com"). Matching
	// ignores case. IP literals are matched exactly.
	Host string
	// Ports the host may be reached on; empty means DefaultPorts.
	Ports []int
	// Upstream, when set, makes Host a gate-terminated route: plain HTTP
	// requests for Host are forwarded over TLS to this "https://host" or
	// "https://host:port" URL, with Inject applied. CONNECT to Host is
	// refused, since the gate could not inject into it. Host may then be
	// any name, including one that does not resolve.
	Upstream string
	// Inject replaces placeholders with credentials in requests the gate
	// can see. It never applies inside a CONNECT tunnel.
	Inject []Injection
}

// Injection swaps a placeholder for a credential in one request header.
type Injection struct {
	// Header is the header to inject into, such as "Authorization".
	Header string
	// Placeholder is the text the sandbox holds in place of the
	// credential. Every occurrence in Header's values is replaced.
	Placeholder string
	// Credential names the credential for the Resolver.
	Credential string
}

// Credential identifies the credential a Resolver must return.
type Credential struct {
	// Grant is the ID the grant was made with.
	Grant string
	// Name is the Injection's Credential.
	Name string
	// Host is where the request is going: the rule's host, or its
	// Upstream's host for a route.
	Host string
}

// Resolver returns the real value of a credential. It is called once per
// request that needs it; cache in it if that is too often. An error fails
// the request with 502 and nothing is forwarded.
type Resolver func(ctx context.Context, c Credential) (string, error)

// Decision is one request's outcome, for an audit log. It never holds a
// credential.
type Decision struct {
	Grant   string // empty when the request had no valid token
	Method  string // CONNECT for tunnels
	Host    string // as the sandbox named it
	Port    int
	Allowed bool
	// Injected lists the headers a credential was injected into.
	Injected []string
	// Reason says why a request was refused or failed.
	Reason string
}

// Config configures a Gate.
type Config struct {
	// Resolve returns credentials for injections. Nil fails every request
	// that needs one.
	Resolve Resolver
	// AllowNonPublic lets the gate dial loopback, private, link-local and
	// other non-public addresses. Leave it false unless an allowed host
	// really is on a private network (and in tests).
	AllowNonPublic bool
	// DialTimeout bounds connecting upstream; zero means 10s.
	DialTimeout time.Duration
	// TLSClientConfig configures the client side of gate-terminated
	// routes, for example extra root CAs. Nil uses the system roots.
	TLSClientConfig *tls.Config
	// OnDecision receives every decision; nil drops them. It must not
	// block.
	OnDecision func(Decision)
}

// Gate is the egress proxy. It is safe for concurrent use.
type Gate struct {
	cfg       Config
	transport *http.Transport
	dialer    *net.Dialer

	mu     sync.RWMutex
	grants map[string]*grant // by token
	ids    map[string]string // grant ID to token
}

type grant struct {
	id     string
	policy Policy
}

// New returns a gate with no grants: it refuses everything.
func New(cfg Config) *Gate {
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	if cfg.OnDecision == nil {
		cfg.OnDecision = func(Decision) {}
	}
	g := &Gate{cfg: cfg, grants: map[string]*grant{}, ids: map[string]string{}}
	g.dialer = &net.Dialer{Timeout: cfg.DialTimeout}
	if !cfg.AllowNonPublic {
		g.dialer.Control = refuseNonPublic
	}
	g.transport = &http.Transport{
		DialContext:           g.dialer.DialContext,
		TLSClientConfig:       cfg.TLSClientConfig.Clone(),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 2 * time.Minute,
		// The gate decides where each request goes; it never consults
		// the environment for another proxy.
		Proxy: nil,
	}
	return g
}

// Grant allows the holder of the returned token to reach what p allows,
// until Revoke(id). id names the grant to the Resolver and in decisions;
// use the run's or the instance's ID.
func (g *Gate) Grant(id string, p Policy) (token string, err error) {
	if id == "" {
		return "", fmt.Errorf("%w: empty grant id", ErrInvalidPolicy)
	}
	if err := p.Validate(); err != nil {
		return "", err
	}
	var b [32]byte
	_, _ = rand.Read(b[:]) // never fails (crypto/rand)
	token = hex.EncodeToString(b[:])
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.ids[id]; ok {
		return "", fmt.Errorf("%w: %q", ErrDuplicateGrant, id)
	}
	g.grants[token] = &grant{id: id, policy: clonePolicy(p)}
	g.ids[id] = token
	return token, nil
}

// Revoke ends a grant. Requests already being forwarded finish; tunnels
// already open stay open until either side closes them.
func (g *Gate) Revoke(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if token, ok := g.ids[id]; ok {
		delete(g.grants, token)
		delete(g.ids, id)
	}
}

// ProxyUser is the user name in the proxy URL; the gate ignores it and
// reads the token from the password.
const ProxyUser = "playscape"

// ProxyURL is the proxy URL a sandbox uses to reach the gate at addr
// ("host:port", as the sandbox sees it) with token.
func ProxyURL(addr, token string) string {
	return (&url.URL{Scheme: schemeHTTP, User: url.UserPassword(ProxyUser, token), Host: addr}).String()
}

// ProxyEnv is the environment that sends a command's HTTP clients through
// the gate: HTTP_PROXY and HTTPS_PROXY in both cases, and an empty
// NO_PROXY. Add it to every isolate.Command of the lease the token was
// granted to.
func ProxyEnv(addr, token string) map[string]string {
	u := ProxyURL(addr, token)
	return map[string]string{
		"HTTP_PROXY": u, "HTTPS_PROXY": u, "http_proxy": u, "https_proxy": u,
		"NO_PROXY": "", "no_proxy": "",
	}
}

// Validate reports whether p is well formed.
func (p Policy) Validate() error {
	for _, r := range p.Allow {
		host := strings.ToLower(r.Host)
		name := strings.TrimPrefix(host, "*.")
		if name == "" || strings.ContainsAny(name, "*/:@ ") && net.ParseIP(strings.Trim(name, "[]")) == nil {
			return fmt.Errorf("%w: host %q", ErrInvalidPolicy, r.Host)
		}
		for _, port := range r.Ports {
			if port <= 0 || port > 65535 {
				return fmt.Errorf("%w: host %q: port %d", ErrInvalidPolicy, r.Host, port)
			}
		}
		if r.Upstream != "" {
			u, err := url.Parse(r.Upstream)
			if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
				return fmt.Errorf("%w: upstream %q: want https://host[:port]", ErrInvalidPolicy, r.Upstream)
			}
			if strings.HasPrefix(host, "*.") {
				return fmt.Errorf("%w: route %q: a route needs an exact host", ErrInvalidPolicy, r.Host)
			}
		}
		for _, inj := range r.Inject {
			if http.CanonicalHeaderKey(inj.Header) == "" || strings.ContainsAny(inj.Header, " :\r\n") ||
				inj.Placeholder == "" || inj.Credential == "" {
				return fmt.Errorf("%w: host %q: injection needs a header, a placeholder and a credential", ErrInvalidPolicy, r.Host)
			}
			if strings.EqualFold(inj.Header, "Host") || strings.HasPrefix(strings.ToLower(inj.Header), "proxy-") {
				return fmt.Errorf("%w: host %q: header %q cannot be injected", ErrInvalidPolicy, r.Host, inj.Header)
			}
		}
	}
	return nil
}

func clonePolicy(p Policy) Policy {
	out := Policy{Allow: make([]Rule, len(p.Allow))}
	for i, r := range p.Allow {
		r.Host = strings.ToLower(r.Host)
		r.Ports = append([]int(nil), r.Ports...)
		r.Inject = append([]Injection(nil), r.Inject...)
		out.Allow[i] = r
	}
	return out
}

// match returns the first rule allowing host:port.
func (p Policy) match(host string, port int) (Rule, bool) {
	host = strings.ToLower(host)
	for _, r := range p.Allow {
		ok := r.Host == host
		if suffix, wild := strings.CutPrefix(r.Host, "*"); wild {
			ok = strings.HasSuffix(host, suffix) && len(host) > len(suffix)
		}
		if !ok {
			continue
		}
		ports := r.Ports
		if len(ports) == 0 {
			ports = DefaultPorts
		}
		for _, allowed := range ports {
			if allowed == port {
				return r, true
			}
		}
	}
	return Rule{}, false
}

// ServeHTTP implements http.Handler: CONNECT tunnels and absolute-form
// plain HTTP requests, both authenticated with a grant's token.
func (g *Gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	gr := g.authenticate(r)
	if gr == nil {
		w.Header().Set("Proxy-Authenticate", `Basic realm="playscape egress"`)
		http.Error(w, "egress: proxy authentication required", http.StatusProxyAuthRequired)
		g.cfg.OnDecision(Decision{Method: r.Method, Host: r.Host, Reason: "no valid token"})
		return
	}
	if r.Method == http.MethodConnect {
		g.connect(w, r, gr)
		return
	}
	g.forward(w, r, gr)
}

// authenticate returns the grant whose token the request carries.
func (g *Gate) authenticate(r *http.Request) *grant {
	auth := r.Header.Get("Proxy-Authorization")
	scheme, encoded, ok := strings.Cut(auth, " ")
	if !ok || !strings.EqualFold(scheme, "Basic") {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil
	}
	_, token, ok := strings.Cut(string(raw), ":")
	if !ok || token == "" {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.grants[token]
}

// splitTarget splits "host:port" (or a bare host, with def as the port).
func splitTarget(hostport string, def int) (string, int, error) {
	host, portStr, err := net.SplitHostPort(hostport)
	if err != nil {
		if !strings.Contains(err.Error(), "missing port") {
			return "", 0, err
		}
		host, portStr = strings.Trim(hostport, "[]"), strconv.Itoa(def)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 || host == "" {
		return "", 0, fmt.Errorf("bad target %q", hostport)
	}
	return host, port, nil
}

// connect relays a CONNECT tunnel to an allowed host. It sees no content,
// so it injects nothing.
func (g *Gate) connect(w http.ResponseWriter, r *http.Request, gr *grant) {
	d := Decision{Grant: gr.id, Method: r.Method, Host: r.Host}
	host, port, err := splitTarget(r.Host, 443)
	if err != nil {
		g.refuse(w, d, http.StatusBadRequest, err.Error())
		return
	}
	d.Host, d.Port = host, port
	rule, ok := gr.policy.match(host, port)
	if !ok {
		g.refuse(w, d, http.StatusForbidden, "host not allowed")
		return
	}
	if rule.Upstream != "" {
		g.refuse(w, d, http.StatusForbidden, "a route takes plain HTTP, not CONNECT")
		return
	}
	upstream, err := g.dialer.DialContext(r.Context(), "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		g.refuse(w, d, http.StatusBadGateway, "dial: "+err.Error())
		return
	}
	defer upstream.Close() //nolint:errcheck // the tunnel is over either way
	rc := http.NewResponseController(w)
	conn, buf, err := rc.Hijack()
	if err != nil {
		g.refuse(w, d, http.StatusInternalServerError, "hijack: "+err.Error())
		return
	}
	defer conn.Close() //nolint:errcheck // the tunnel is over either way
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	d.Allowed = true
	g.cfg.OnDecision(d)
	done := make(chan struct{}, 2)
	go func() {
		// Bytes the client sent after its CONNECT are buffered.
		if n := buf.Reader.Buffered(); n > 0 {
			pending, _ := buf.Peek(n)
			if _, err := upstream.Write(pending); err != nil {
				done <- struct{}{}
				return
			}
		}
		_, _ = io.Copy(upstream, conn)
		closeWrite(upstream)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(conn, upstream)
		closeWrite(conn)
		done <- struct{}{}
	}()
	<-done
	<-done
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// forward handles a plain HTTP request through the proxy, injecting
// credentials for an allowed host.
func (g *Gate) forward(w http.ResponseWriter, r *http.Request, gr *grant) {
	d := Decision{Grant: gr.id, Method: r.Method, Host: r.Host}
	// A proxy request carries the absolute URL; the Host header is not
	// trusted to say where it goes.
	if !r.URL.IsAbs() || r.URL.Scheme != schemeHTTP || r.URL.Host == "" {
		g.refuse(w, d, http.StatusBadRequest, "want an absolute http:// URL; use CONNECT for https")
		return
	}
	host, port, err := splitTarget(r.URL.Host, 80)
	if err != nil {
		g.refuse(w, d, http.StatusBadRequest, err.Error())
		return
	}
	d.Host, d.Port = host, port
	rule, ok := gr.policy.match(host, port)
	if !ok {
		g.refuse(w, d, http.StatusForbidden, "host not allowed")
		return
	}
	target := &url.URL{Scheme: schemeHTTP, Host: r.URL.Host}
	if rule.Upstream != "" {
		target, _ = url.Parse(rule.Upstream) // validated at Grant
		target.Path = ""
	}
	// Resolve every credential before anything is sent.
	values := make([]string, len(rule.Inject))
	for i, inj := range rule.Inject {
		v, err := g.resolve(r.Context(), Credential{Grant: gr.id, Name: inj.Credential, Host: target.Hostname()})
		if err != nil {
			g.refuse(w, d, http.StatusBadGateway, "credential "+inj.Credential+": "+err.Error())
			return
		}
		values[i] = v
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = target.Scheme, target.Host
			pr.Out.Host = target.Host
			pr.Out.Header.Del("Proxy-Authorization")
			pr.Out.Header.Del("Proxy-Connection")
			for i, inj := range rule.Inject {
				vals := pr.Out.Header.Values(inj.Header)
				hit := false
				for j, v := range vals {
					if strings.Contains(v, inj.Placeholder) {
						vals[j] = strings.ReplaceAll(v, inj.Placeholder, values[i])
						hit = true
					}
				}
				if hit {
					d.Injected = append(d.Injected, http.CanonicalHeaderKey(inj.Header))
				}
			}
		},
		Transport: g.transport,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			d.Allowed, d.Reason = true, "upstream: "+err.Error()
			http.Error(w, "egress: upstream failed", http.StatusBadGateway)
		},
	}
	d.Allowed = true
	rp.ServeHTTP(w, r)
	g.cfg.OnDecision(d)
}

func (g *Gate) resolve(ctx context.Context, c Credential) (string, error) {
	if g.cfg.Resolve == nil {
		return "", errors.New("no resolver")
	}
	return g.cfg.Resolve(ctx, c)
}

func (g *Gate) refuse(w http.ResponseWriter, d Decision, status int, reason string) {
	d.Reason = reason
	g.cfg.OnDecision(d)
	msg := "egress: " + reason
	if status == http.StatusForbidden {
		msg = ErrDenied.Error() + ": " + reason
	}
	http.Error(w, msg, status)
}

// refuseNonPublic is a net.Dialer Control func that refuses every address
// that is not a public unicast one. It runs after name resolution, on the
// address actually dialed, so no name can lead around it.
func refuseNonPublic(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !public(ip) {
		return fmt.Errorf("%w: %s", ErrNonPublicAddress, host)
	}
	return nil
}

func public(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	if ip4 := ip.To4(); ip4 != nil {
		// 100.64.0.0/10 (carrier-grade NAT) and 0.0.0.0/8.
		if ip4[0] == 100 && ip4[1]&0xc0 == 64 || ip4[0] == 0 {
			return false
		}
	}
	return true
}
