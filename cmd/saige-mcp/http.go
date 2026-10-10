package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// HTTP defaults.
const (
	defaultAddr     = "127.0.0.1:8765"
	defaultPath     = "/mcp"
	defaultTokenEnv = "SAIGE_MCP_TOKEN" //nolint:gosec // the name of the variable, not a credential
	// minTokenLength rejects tokens short enough to guess.
	minTokenLength = 16
	// maxRequestBody bounds one JSON-RPC request.
	maxRequestBody = 4 << 20
	// sessionTimeout closes sessions idle this long.
	sessionTimeout = 30 * time.Minute
)

// httpFlags are the --transport http flags.
type httpFlags struct {
	addr, path, tokenEnv, tokenFile, tlsCert, tlsKey string
	allowInsecure                                    bool
	rate                                             float64
	burst                                            int
}

// httpConfig is a checked HTTP configuration.
type httpConfig struct {
	addr, path        string
	tlsCert, tlsKey   string
	tokens            []string
	rate              float64
	burst             int
	sessionTimeout    time.Duration
	now               func() time.Time
	readHeaderTimeout time.Duration
}

func (c httpConfig) tls() bool { return c.tlsCert != "" }

func (c httpConfig) endpoint() string {
	scheme := "http"
	if c.tls() {
		scheme = "https"
	}
	return scheme + "://" + c.addr + c.path
}

// newHTTPConfig checks the flags. It needs at least one bearer token, and
// it refuses plain HTTP on an address other than loopback unless
// allowInsecure says a TLS-terminating proxy sits in front.
func newHTTPConfig(f httpFlags, getenv func(string) string) (httpConfig, error) {
	cfg := httpConfig{addr: f.addr, path: f.path, tlsCert: f.tlsCert, tlsKey: f.tlsKey,
		rate: f.rate, burst: f.burst, sessionTimeout: sessionTimeout, readHeaderTimeout: 10 * time.Second}
	if cfg.path == "" || cfg.path[0] != '/' {
		return cfg, fmt.Errorf("--path %q must start with /", f.path)
	}
	if (f.tlsCert == "") != (f.tlsKey == "") {
		return cfg, errors.New("--tls-cert and --tls-key go together")
	}
	if f.rate <= 0 || f.burst <= 0 {
		return cfg, errors.New("--rate-limit and --rate-burst must be positive")
	}
	if !cfg.tls() && !f.allowInsecure {
		loopback, err := isLoopback(f.addr)
		if err != nil {
			return cfg, err
		}
		if !loopback {
			return cfg, fmt.Errorf("refusing plain HTTP on %s: serve TLS with --tls-cert and --tls-key, bind a loopback address, "+
				"or pass --allow-insecure-bind when a TLS-terminating proxy is in front", f.addr)
		}
	}
	tokens, err := loadTokens(f.tokenEnv, f.tokenFile, getenv)
	if err != nil {
		return cfg, err
	}
	cfg.tokens = tokens
	return cfg, nil
}

// isLoopback reports whether addr binds only a loopback interface. An
// empty host binds every interface.
func isLoopback(addr string) (bool, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false, fmt.Errorf("--addr %q: %w", addr, err)
	}
	if host == "localhost" {
		return true, nil
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback(), nil
}

// loadTokens reads the bearer tokens: comma-separated in the environment
// variable, and one per line in the file, where blank lines and lines
// starting with # are skipped.
func loadTokens(envName, file string, getenv func(string) string) ([]string, error) {
	var tokens []string
	if envName != "" {
		for _, t := range strings.Split(getenv(envName), ",") {
			if t = strings.TrimSpace(t); t != "" {
				tokens = append(tokens, t)
			}
		}
	}
	if file != "" {
		raw, err := os.ReadFile(file) //nolint:gosec // the path is the operator's own flag
		if err != nil {
			return nil, fmt.Errorf("--token-file: %w", err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
				tokens = append(tokens, line)
			}
		}
	}
	if len(tokens) == 0 {
		return nil, fmt.Errorf("--transport http needs a bearer token: set $%s or --token-file", envName)
	}
	for _, t := range tokens {
		if len(t) < minTokenLength {
			return nil, fmt.Errorf("a bearer token is shorter than %d characters", minTokenLength)
		}
	}
	return tokens, nil
}

// tokenID names a token in logs and rate-limit keys without revealing it.
func tokenID(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "token-" + hex.EncodeToString(sum[:4])
}

// verifier accepts exactly the configured tokens. Tokens are compared as
// SHA-256 digests in constant time, so neither the content nor the length
// of a token leaks through timing.
func verifier(tokens []string) auth.TokenVerifier {
	type known struct {
		digest [32]byte
		id     string
	}
	set := make([]known, len(tokens))
	for i, t := range tokens {
		set[i] = known{digest: sha256.Sum256([]byte(t)), id: tokenID(t)}
	}
	return func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		got := sha256.Sum256([]byte(token))
		match := ""
		for _, k := range set {
			if subtle.ConstantTimeCompare(got[:], k.digest[:]) == 1 {
				match = k.id
			}
		}
		if match == "" {
			return nil, auth.ErrInvalidToken
		}
		// The tokens are static; the expiration only satisfies the
		// middleware, which requires one.
		return &auth.TokenInfo{UserID: match, Expiration: time.Now().Add(time.Hour)}, nil
	}
}

// rateLimiter is a token bucket per bearer token.
type rateLimiter struct {
	rate, burst float64
	now         func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(rate float64, burst int, now func() time.Time) *rateLimiter {
	if now == nil {
		now = time.Now
	}
	return &rateLimiter{rate: rate, burst: float64(burst), now: now, buckets: map[string]*bucket{}}
}

// allow takes one request from key's bucket. When the bucket is empty it
// returns false and how long until the next request is allowed.
func (l *rateLimiter) allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
}

// middleware rejects a request over its token's rate with 429. It runs
// after authentication, so the key is a verified token.
func (l *rateLimiter) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := ""
		if info := auth.TokenInfoFromContext(r.Context()); info != nil {
			key = info.UserID
		}
		if ok, wait := l.allow(key); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// httpHandler serves server at cfg.path behind bearer authentication and
// the per-token rate limit. Everything else is 404.
func httpHandler(server *mcp.Server, cfg httpConfig) http.Handler {
	streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{SessionTimeout: cfg.sessionTimeout})
	limited := newRateLimiter(cfg.rate, cfg.burst, cfg.now).middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		streamable.ServeHTTP(w, r)
	}))
	mux := http.NewServeMux()
	mux.Handle(cfg.path, auth.RequireBearerToken(verifier(cfg.tokens), nil)(limited))
	return mux
}

// serveHTTP serves until ctx ends, then shuts down gracefully.
func serveHTTP(ctx context.Context, server *mcp.Server, cfg httpConfig) error {
	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           httpHandler(server, cfg),
		ReadHeaderTimeout: cfg.readHeaderTimeout,
		ErrorLog:          log.Default(),
	}
	errc := make(chan error, 1)
	go func() {
		if cfg.tls() {
			errc <- srv.ListenAndServeTLS(cfg.tlsCert, cfg.tlsKey)
		} else {
			errc <- srv.ListenAndServe()
		}
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
		if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}
