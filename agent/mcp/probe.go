package mcp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ProbeKind classifies a probe outcome.
type ProbeKind string

// Probe outcomes.
const (
	ProbeOK ProbeKind = "ok"
	// ProbeUnreachable means nothing answered (DNS, refused, missing executable).
	ProbeUnreachable ProbeKind = "unreachable"
	// ProbeAuthRejected means the server answered 401 or 403.
	ProbeAuthRejected ProbeKind = "auth_rejected"
	// ProbeBlocked means SafeHTTPClient refused the destination address.
	ProbeBlocked ProbeKind = "blocked"
	// ProbeTimeout means the handshake or listing ran out of time.
	ProbeTimeout ProbeKind = "timeout"
	// ProbeProtocol means something answered but did not complete MCP.
	ProbeProtocol ProbeKind = "protocol"
)

// probeToolLimit caps ProbeResult.Tools; the count is always complete.
const probeToolLimit = 25

// probeBodyLimit caps how much of a failed response body is kept.
const probeBodyLimit = 512

// ProbeResult is a structured health check of one server.
type ProbeResult struct {
	OK      bool
	Kind    ProbeKind
	Message string
	// ToolCount is the number of tools advertised; Tools lists at most 25
	// names.
	ToolCount int
	Tools     []string
	Latency   time.Duration
	// Status is the last non-success HTTP status seen, for remote servers.
	Status int
}

// Probe connects to a server, lists its tools and disconnects. It never
// returns an error: every failure is described by the result, so a caller can
// show "test connection" output without parsing error strings. For a remote
// server whose handshake fails with an HTTP error, Message includes the start
// of the response body, which usually carries the vendor's real reason.
func Probe(ctx context.Context, spec ServerSpec) ProbeResult {
	start := time.Now()
	if err := spec.Validate(); err != nil {
		return ProbeResult{Kind: ProbeProtocol, Message: err.Error()}
	}

	rec := &statusRecorder{}
	if !spec.IsLocal() {
		hc := &http.Client{}
		base := http.DefaultTransport
		if spec.HTTPClient != nil {
			*hc = *spec.HTTPClient
			if spec.HTTPClient.Transport != nil {
				base = spec.HTTPClient.Transport
			}
		}
		rec.base = base
		hc.Transport = rec
		spec.HTTPClient = hc
	}

	res := ProbeResult{}
	c, err := Connect(ctx, spec)
	if err == nil {
		defer func() { _ = c.Close(ctx) }()
		var cat Catalog
		cat, err = c.Catalog(ctx)
		if err == nil {
			res.OK, res.Kind = true, ProbeOK
			res.ToolCount = len(cat.Tools)
			for i, t := range cat.Tools {
				if i == probeToolLimit {
					break
				}
				res.Tools = append(res.Tools, t.Name)
			}
		}
	}
	res.Latency = time.Since(start)
	res.Status, _ = rec.last()
	if err != nil {
		res.Kind = classifyProbe(ctx, err, res.Status)
		res.Message = err.Error()
		if _, body := rec.last(); body != "" {
			res.Message += ": " + body
		}
	}
	return res
}

func classifyProbe(ctx context.Context, err error, status int) ProbeKind {
	var netErr net.Error
	var dnsErr *net.DNSError
	var opErr *net.OpError
	switch {
	case errors.Is(err, ErrBlockedAddress) || strings.Contains(err.Error(), ErrBlockedAddress.Error()):
		return ProbeBlocked
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return ProbeAuthRejected
	case errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil:
		return ProbeTimeout
	case errors.As(err, &netErr) && netErr.Timeout():
		return ProbeTimeout
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, fs.ErrNotExist), errors.As(err, &dnsErr), errors.As(err, &opErr):
		return ProbeUnreachable
	}
	// The SDK formats some transport failures into its message rather than
	// wrapping them, so the common ones are also recognized by text.
	msg := err.Error()
	for _, s := range unreachableText {
		if strings.Contains(msg, s) {
			return ProbeUnreachable
		}
	}
	return ProbeProtocol
}

var unreachableText = []string{
	"connection refused", "no such host", "network is unreachable", "no route to host",
	"no such file or directory", "executable file not found",
}

// statusRecorder remembers the last failed HTTP response and the start of
// its body, then hands the body on unchanged.
type statusRecorder struct {
	base   http.RoundTripper
	mu     sync.Mutex
	status int
	body   string
}

func (s *statusRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := s.base.RoundTrip(req)
	if err != nil || resp.StatusCode < 400 {
		return resp, err
	}
	head, _ := io.ReadAll(io.LimitReader(resp.Body, probeBodyLimit))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), resp.Body), resp.Body}
	s.mu.Lock()
	s.status = resp.StatusCode
	s.body = strings.TrimSpace(string(head))
	s.mu.Unlock()
	return resp, nil
}

func (s *statusRecorder) last() (int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, s.body
}
