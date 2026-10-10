// Package mcp connects an agent to Model Context Protocol servers and exposes
// their tools as ordinary types.Tool values.
//
// This is the client side. The saige-mcp binary is the server side: it exposes
// this SDK's tools to other MCP clients. The two are unrelated at runtime.
//
// # Local versus remote
//
// The two transports are not interchangeable, and the differences are
// operational rather than cosmetic:
//
//	                 local (stdio)                 remote (streamable HTTP)
//	process          we spawn and hold it          someone else runs it
//	lifetime         dies when we Close            outlives us; may restart
//	failure mode     process exit                  network; some rejections retryable
//	auth             environment variables         headers, bearer tokens
//	trust            code we chose to run          a third party
//	latency          microseconds                  a network round trip
//	tool list        stable per process            can change between calls
//
// The lifetime difference is the one that bites. A local server is a child
// process this package owns: failing to Close it leaks the process for the
// lifetime of the agent. A remote server needs none of that custody, but every
// call can fail transiently and its advertised tool list can change under you
// between connections, which is why AllowedTools and a gate matter far more
// there.
//
// # Failure handling
//
// A call that finds its session dead (a crashed local process, or a remote
// session the server forgot) reconnects once. The call itself is repeated only
// when the tool is known to be idempotent, because the first attempt may have
// run: it is listed in IdempotentTools, or its annotations say so and the spec
// sets TrustHints. Throttled HTTP requests are retried per ServerSpec.Retry
// under the same rule: a rejection before the server did anything is always
// safe to repeat, an ambiguous gateway error is repeated only for reads and
// idempotent tools. Application errors never trigger a reconnect.
//
// # Provider-side remote MCP is a third thing
//
// Some providers connect to a remote MCP server themselves (see
// types.RemoteMCPServer). That path never reaches this package: the calls
// happen inside the provider, so no local ToolGate sees them, no
// ToolExecStartDelta is emitted, and the authorization token is handed to the
// provider rather than kept here. Connecting through this package instead
// makes every MCP tool a normal local tool: gateable, logged, and durable
// through the StepRunner. Prefer it unless the provider-side path buys
// something specific.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/urmzd/saige/agent/types"
)

// DefaultCallTimeout bounds a single tool call when the spec sets none. An MCP
// server that hangs would otherwise hang the agent iteration with it.
const DefaultCallTimeout = 60 * time.Second

// DefaultConnectTimeout bounds the initial handshake.
const DefaultConnectTimeout = 30 * time.Second

// ServerSpec describes one MCP server to connect to. Set either Command (local
// stdio) or URL (remote streamable HTTP), never both.
type ServerSpec struct {
	// Name identifies the server in tool names, logs and errors. Required.
	Name string

	// ── Local transport ─────────────────────────────────────────────

	// Command is the executable to spawn. Setting it selects the local
	// transport, and this package owns the resulting process until Close.
	Command string
	// Args are the command arguments.
	Args []string
	// Env is the child process environment in "KEY=VALUE" form. Empty inherits
	// this process's environment, which is convenient and also how credentials
	// leak into a server that did not need them; set it explicitly for
	// anything untrusted.
	Env []string
	// WorkDir is the child's working directory. Empty inherits.
	WorkDir string

	// ── Remote transport ────────────────────────────────────────────

	// URL is the server endpoint. Setting it selects the remote transport.
	URL string
	// Headers are sent on every request, for static bearer tokens and the
	// like. Use TokenFunc or OAuthHandler for credentials that expire.
	Headers map[string]string
	// HTTPClient overrides the transport's client, for custom TLS, proxies, or
	// SafeHTTPClient's address guard.
	HTTPClient *http.Client
	// TokenFunc returns a bearer token for each request, so a credential can
	// rotate or refresh during a long session. Its error fails the request.
	TokenFunc func(context.Context) (string, error)
	// OAuthHandler runs the MCP authorization flow (authorization code, PKCE,
	// dynamic client registration) and retries a request rejected with 401 or
	// 403 once it has a token. See the go-sdk auth package for implementations.
	OAuthHandler auth.OAuthHandler
	// Retry repeats HTTP requests the server rejected as throttled or
	// unavailable. nil disables it. See RetryPolicy for which requests qualify.
	Retry *RetryPolicy
	// ConnectRetry repeats the handshake when the server throttles or fails
	// it. The handshake has no side effects, so any 429 or 5xx qualifies.
	// nil disables it.
	ConnectRetry *RetryPolicy
	// MaxReconnects bounds how often a dropped event stream is resumed. Zero
	// uses the transport default (5); negative disables resumption.
	MaxReconnects int

	// ── Common ──────────────────────────────────────────────────────

	// ToolPrefix is prepended to every imported tool name to keep two servers
	// exposing "search" from colliding. Empty defaults to Name + "_"; set it to
	// "-" to import names unchanged. Unprefixed names can collide with other
	// servers and with local tools; registration refuses either collision.
	ToolPrefix string
	// AllowedTools restricts which of the server's tools are imported at all.
	// Empty imports everything advertised, which for a remote server means
	// trusting a list that can change between connections.
	AllowedTools []string
	// Gate is applied to this server's tools. Compose it into the agent's gate
	// via Pool.Gate. nil means no server-specific policy.
	Gate types.ToolGate
	// TrustHints lets the server's tool annotations lower a tool's capability
	// class: readOnlyHint becomes ToolCapabilityRead and destructiveHint=false
	// becomes ToolCapabilityWrite. Without it only hints that raise the class
	// are honored, because an untrusted server can label a write as a read.
	TrustHints bool
	// IdempotentTools names remote tools (before prefixing) that are safe to
	// call twice. They are retried after an ambiguous failure. With
	// TrustHints, so are tools whose annotations claim readOnlyHint or
	// idempotentHint.
	IdempotentTools []string
	// ConnectTimeout and CallTimeout bound the handshake and each call.
	ConnectTimeout time.Duration
	CallTimeout    time.Duration
	// KeepAlive pings the server at this interval and closes the session when
	// a ping fails, so the next call reconnects instead of waiting on a dead
	// connection. Zero disables it.
	KeepAlive time.Duration
	// MaxConcurrent bounds in-flight calls to this server. Excess calls wait,
	// within their own timeout. Zero means unlimited.
	MaxConcurrent int
	// MaxResultBytes caps the text in one tool result: text blocks, embedded
	// resource text, and structured content. Zero uses DefaultMaxResultBytes;
	// negative disables the cap.
	MaxResultBytes int
	// MaxBinaryBytes caps image, audio and blob bytes in one tool result.
	// Zero uses DefaultMaxBinaryBytes; negative disables the cap.
	MaxBinaryBytes int
	// ArgTransform rewrites arguments before each call. It receives the
	// remote tool name, the tool's raw input schema, and a copy of the
	// arguments. CoerceScalarsToArrays is a ready-made transform.
	ArgTransform func(tool string, schema json.RawMessage, args map[string]any) map[string]any
	// OnToolsChanged runs when the server announces that its tool list
	// changed. The cached catalog is already invalidated; call Pool.Refresh to
	// apply the change to a registry. It runs on its own goroutine.
	OnToolsChanged func(server string)
	// OnProgress receives progress notifications for calls whose context has
	// no handler from WithProgress.
	OnProgress func(context.Context, Progress)
}

// Local builds a spec for a server this process spawns and holds.
func Local(name, command string, args ...string) ServerSpec {
	return ServerSpec{Name: name, Command: command, Args: args}
}

// Remote builds a spec for a server reached over HTTP.
func Remote(name, url string) ServerSpec {
	return ServerSpec{Name: name, URL: url}
}

// IsLocal reports whether the spec uses the local stdio transport.
func (s ServerSpec) IsLocal() bool { return s.Command != "" }

// Validate reports whether the spec can be connected.
func (s ServerSpec) Validate() error {
	if s.Name == "" {
		return fmt.Errorf("mcp: server spec needs a Name")
	}
	switch {
	case s.Command == "" && s.URL == "":
		return fmt.Errorf("mcp: server %q needs either Command (local) or URL (remote)", s.Name)
	case s.Command != "" && s.URL != "":
		return fmt.Errorf("mcp: server %q sets both Command and URL; pick one transport", s.Name)
	}
	if s.IsLocal() && (s.TokenFunc != nil || s.OAuthHandler != nil) {
		return fmt.Errorf("mcp: server %q is local; TokenFunc and OAuthHandler apply only to remote servers", s.Name)
	}
	if s.TokenFunc != nil && s.OAuthHandler != nil {
		return fmt.Errorf("mcp: server %q sets both TokenFunc and OAuthHandler; pick one credential source", s.Name)
	}
	if s.TokenFunc != nil || s.OAuthHandler != nil {
		for k := range s.Headers {
			if strings.EqualFold(k, "Authorization") {
				return fmt.Errorf("mcp: server %q sets an Authorization header and a dynamic credential; remove the header", s.Name)
			}
		}
	}
	for _, p := range []*RetryPolicy{s.Retry, s.ConnectRetry} {
		if err := p.validate(); err != nil {
			return fmt.Errorf("mcp: server %q: %w", s.Name, err)
		}
	}
	if s.MaxConcurrent < 0 {
		return fmt.Errorf("mcp: server %q: MaxConcurrent must not be negative", s.Name)
	}
	return nil
}

func (s ServerSpec) prefix() string {
	switch s.ToolPrefix {
	case "":
		return s.Name + "_"
	case "-":
		return ""
	default:
		return s.ToolPrefix
	}
}

func (s ServerSpec) callTimeout() time.Duration {
	if s.CallTimeout > 0 {
		return s.CallTimeout
	}
	return DefaultCallTimeout
}

// Client is a live connection to one MCP server.
//
// For a local server the connection owns a child process, so Close is
// mandatory rather than tidy: skipping it leaks the process. For a remote
// server Close only releases the session.
type Client struct {
	spec ServerSpec
	sem  chan struct{} // nil when MaxConcurrent is zero

	// dialMu serializes reconnects so concurrent calls that all see the same
	// dead session produce one new session, not one each.
	dialMu sync.Mutex

	mu         sync.RWMutex
	session    *mcpsdk.ClientSession
	closed     bool
	tools      map[string]*serverTool // last exposed set, by prefixed name
	withdrawn  map[string]bool        // remote names a listing dropped from tools
	catalog    *Catalog               // nil until listed or after a change notice
	catalogGen uint64                 // bumped on every invalidation
	byName     map[string]CatalogTool // catalog tools by remote name

	progressSeq atomic.Uint64
	progress    sync.Map // progress token -> func(Progress)

	// closing is cancelled when Close starts. In-flight calls derive from it,
	// because the session's Close waits for them and a call blocked on a hung
	// server would otherwise hold Close forever.
	closing context.Context
	cancel  context.CancelFunc
	// calls counts in-flight tool calls so Close can let them unwind before
	// closing the session.
	calls sync.WaitGroup
}

// Connect dials the server and completes the MCP handshake. The caller owns
// the returned Client and must Close it.
func Connect(ctx context.Context, spec ServerSpec) (*Client, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	c := &Client{spec: spec, tools: map[string]*serverTool{}}
	c.closing, c.cancel = context.WithCancel(context.Background())
	if spec.MaxConcurrent > 0 {
		c.sem = make(chan struct{}, spec.MaxConcurrent)
	}
	session, err := c.dial(ctx)
	if err != nil {
		c.cancel()
		return nil, err
	}
	c.session = session
	return c, nil
}

// dial builds a transport and completes the handshake. Every connection gets
// a fresh transport: a CommandTransport cannot be reused, and a fresh HTTP
// transport starts with no stale session ID.
func (c *Client) dial(ctx context.Context) (*mcpsdk.ClientSession, error) {
	spec := c.spec
	timeout := spec.ConnectTimeout
	if timeout <= 0 {
		timeout = DefaultConnectTimeout
	}
	connectCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "saige", Version: "1"}, &mcpsdk.ClientOptions{
		KeepAlive:                   spec.KeepAlive,
		ToolListChangedHandler:      c.onToolListChanged,
		ProgressNotificationHandler: c.onProgress,
	})

	var transport mcpsdk.Transport
	if spec.IsLocal() {
		// The command must NOT take connectCtx: that context is cancelled when
		// the handshake finishes, which would kill the server we just started.
		transport = &mcpsdk.CommandTransport{Command: localCommand(spec)}
	} else {
		transport = &mcpsdk.StreamableClientTransport{
			Endpoint:     spec.URL,
			HTTPClient:   c.httpClient(),
			MaxRetries:   spec.MaxReconnects,
			OAuthHandler: spec.OAuthHandler,
		}
	}

	session, err := client.Connect(connectCtx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: connect to %q: %w", spec.Name, err)
	}
	return session, nil
}

// httpClient layers credentials and retries over the configured client. The
// credential layer sits below retry so every attempt resolves a fresh token.
func (c *Client) httpClient() *http.Client {
	spec := c.spec
	if len(spec.Headers) == 0 && spec.TokenFunc == nil && spec.Retry == nil && spec.ConnectRetry == nil {
		return spec.HTTPClient
	}
	base := http.DefaultTransport
	out := &http.Client{}
	if spec.HTTPClient != nil {
		*out = *spec.HTTPClient
		if spec.HTTPClient.Transport != nil {
			base = spec.HTTPClient.Transport
		}
	}
	if len(spec.Headers) > 0 || spec.TokenFunc != nil {
		base = &headerTransport{base: base, headers: spec.Headers, token: spec.TokenFunc}
	}
	if spec.Retry != nil || spec.ConnectRetry != nil {
		base = &retryTransport{
			base:       base,
			server:     spec.Name,
			call:       spec.Retry,
			connect:    spec.ConnectRetry,
			idempotent: c.isIdempotent,
		}
	}
	out.Transport = base
	return out
}

// Spec returns the spec this client was built from.
func (c *Client) Spec() ServerSpec { return c.spec }

// IsLocal reports whether this client holds a child process.
func (c *Client) IsLocal() bool { return c.spec.IsLocal() }

// Tools lists the server's tools, filtered by AllowedTools, wrapped as
// types.Tool with prefixed names.
//
// The list comes from the cached catalog, which the server's
// tools/list_changed notification invalidates. A tool the server stopped
// offering stays a valid Go value, but calling it returns an error result
// rather than reaching the server, until a later listing offers it again.
func (c *Client) Tools(ctx context.Context) ([]types.Tool, error) {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}

	allowed := map[string]bool{}
	for _, n := range c.spec.AllowedTools {
		allowed[n] = true
	}
	idempotent := map[string]bool{}
	for _, n := range c.spec.IdempotentTools {
		idempotent[n] = true
	}

	prefix := c.spec.prefix()
	out := make([]types.Tool, 0, len(cat.Tools))
	next := make(map[string]*serverTool, len(cat.Tools))

	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range cat.Tools {
		if len(allowed) > 0 && !allowed[t.Name] {
			continue
		}
		st := &serverTool{
			client:     c,
			remote:     t.Name,
			def:        c.toolDef(prefix+t.Name, t),
			schema:     t.InputSchema,
			timeout:    c.spec.callTimeout(),
			server:     c.spec.Name,
			idempotent: idempotent[t.Name] || c.hintsIdempotent(t),
		}
		next[st.def.Name] = st
		out = append(out, st)
	}
	// Tools exposed before but missing now are withdrawn: a call to one
	// returns an error result even after the catalog is dropped again, so a
	// stale registration cannot reach the server while no listing is cached.
	if c.withdrawn == nil {
		c.withdrawn = map[string]bool{}
	}
	for name, st := range c.tools {
		if _, ok := next[name]; !ok {
			c.withdrawn[st.remote] = true
		}
	}
	for _, st := range next {
		delete(c.withdrawn, st.remote)
	}
	c.tools = next
	return out, nil
}

// Register lists the server's tools and adds them to a registry, returning how
// many were added. A name already held by a tool that this client did not
// produce is an error: a remote server must never silently replace a local
// tool and inherit the model's calls to it.
func (c *Client) Register(ctx context.Context, registry *types.ToolRegistry) (int, error) {
	tools, err := c.Tools(ctx)
	if err != nil {
		return 0, err
	}
	for _, t := range tools {
		if err := registerChecked(registry, t, c); err != nil {
			return 0, err
		}
	}
	return len(tools), nil
}

// useSession returns the live session for one request, registered as an
// in-flight call, with ctx cancelled when Close starts. Every session user
// other than tool calls goes through it: a request blocked on a hung server
// would otherwise hold the session's Close, and Close would give up without
// releasing a local child process. The caller must call done.
func (c *Client) useSession(ctx context.Context) (context.Context, *mcpsdk.ClientSession, func(), error) {
	session, err := c.beginCall()
	if err != nil {
		return ctx, nil, nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.closing, cancel)
	return ctx, session, func() {
		stop()
		cancel()
		c.calls.Done()
	}, nil
}

// beginCall returns the live session and registers an in-flight call, which
// the caller ends with c.calls.Done. Registration happens under the same lock
// Close takes to clear the session, so Close never waits on a call that
// started after it. It also makes a Close racing the call return a
// closed-server error rather than a nil session.
func (c *Client) beginCall() (*mcpsdk.ClientSession, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.session == nil {
		return nil, fmt.Errorf("mcp: server %q is closed", c.spec.Name)
	}
	c.calls.Add(1)
	return c.session, nil
}

// reconnect replaces a dead session. It returns the current session without
// dialing when another caller already replaced dead.
func (c *Client) reconnect(ctx context.Context, dead *mcpsdk.ClientSession) (*mcpsdk.ClientSession, error) {
	c.dialMu.Lock()
	defer c.dialMu.Unlock()

	c.mu.RLock()
	current, closed := c.session, c.closed
	c.mu.RUnlock()
	if closed {
		return nil, fmt.Errorf("mcp: server %q is closed", c.spec.Name)
	}
	if current != dead && current != nil {
		return current, nil
	}

	_ = dead.Close() // reaps a crashed local process; the error is the crash
	session, err := c.dial(context.WithoutCancel(ctx))
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		_ = session.Close()
		return nil, fmt.Errorf("mcp: server %q is closed", c.spec.Name)
	}
	c.session = session
	// A new session may advertise a different tool list.
	c.invalidateLocked()
	c.mu.Unlock()
	return session, nil
}

func (c *Client) isClosed() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.closed
}

// Ping checks that the session answers.
func (c *Client) Ping(ctx context.Context) error {
	ctx, session, done, err := c.useSession(ctx)
	if err != nil {
		return err
	}
	defer done()
	if err := session.Ping(ctx, nil); err != nil {
		return fmt.Errorf("mcp: ping %q: %w", c.spec.Name, err)
	}
	return nil
}

// Close ends the session and, for a local server, waits for the child process
// to exit. Safe to call more than once.
func (c *Client) Close() error {
	// Guarded: Close races a concurrent CallTool, which reads c.session on the
	// same mutex that protects the tool map.
	c.mu.Lock()
	session := c.session
	c.session = nil
	c.closed = true
	c.mu.Unlock()

	if session == nil {
		return nil
	}

	// Cancel in-flight calls and let them unwind first: the SDK's Close waits
	// for them, and a call blocked on a server that never answers would
	// otherwise hold Close forever.
	c.cancel()
	deadline := time.NewTimer(closeTimeout)
	defer deadline.Stop()
	unwound := make(chan struct{})
	go func() { c.calls.Wait(); close(unwound) }()
	select {
	case <-unwound:
	case <-deadline.C:
		return fmt.Errorf("mcp: close %q: in-flight calls did not stop within %v", c.spec.Name, closeTimeout)
	}

	// Closing the session closes the transport, and for a local server the
	// SDK's CommandTransport signals, kills and reaps the child process.
	done := make(chan error, 1)
	go func() { done <- session.Close() }()
	select {
	case err := <-done:
		return err
	case <-deadline.C:
		return fmt.Errorf("mcp: close %q: server did not release its session within %v", c.spec.Name, closeTimeout)
	}
}

// closeTimeout bounds Close. Variable for tests.
var closeTimeout = 10 * time.Second

// onToolListChanged drops the cached catalog so the next listing re-fetches,
// then tells the host.
func (c *Client) onToolListChanged(context.Context, *mcpsdk.ToolListChangedRequest) {
	c.mu.Lock()
	c.invalidateLocked()
	c.mu.Unlock()
	if fn := c.spec.OnToolsChanged; fn != nil {
		// The handler runs on the session's read loop; a callback that calls
		// back into the session must not block it.
		go fn(c.spec.Name)
	}
}

// toolDef converts an MCP tool declaration into this SDK's ToolDef. The MCP
// input schema arrives as free-form JSON, so unconvertible schemas degrade to
// an empty object rather than failing the import: a tool with a schema we
// cannot read is still callable, just not well described.
func (c *Client) toolDef(name string, t CatalogTool) types.ToolDef {
	desc := t.Description
	if desc == "" {
		desc = "MCP tool " + t.Name + " on server " + c.spec.Name
	}
	return types.ToolDef{
		Name:        name,
		Description: desc,
		Parameters:  schemaFromMCP(json.RawMessage(t.InputSchema)),
		Capability:  CapabilityFromAnnotations(t.Annotations, c.spec.TrustHints),
	}
}

// offers reports whether the server still advertises a remote tool. Before
// the catalog is known, or while it is being refreshed, a tool counts as
// offered unless an earlier listing withdrew it, and the server is the judge.
func (c *Client) offers(remote string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.catalog == nil {
		return !c.withdrawn[remote]
	}
	_, ok := c.byName[remote]
	return ok
}

// isIdempotent reports whether a remote tool may be repeated after an
// ambiguous failure.
func (c *Client) isIdempotent(remote string) bool {
	for _, n := range c.spec.IdempotentTools {
		if n == remote {
			return true
		}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.byName[remote]
	return ok && c.hintsIdempotent(t)
}

// hintsIdempotent reports whether a tool's annotations allow repeating it.
// Repeating a call after an ambiguous failure can apply a write twice in the
// user's account on that server, so the claim counts only with TrustHints.
func (c *Client) hintsIdempotent(t CatalogTool) bool {
	return c.spec.TrustHints && (t.readOnly() || t.idempotent())
}

// ── Tool wrapper ────────────────────────────────────────────────────

// serverTool exposes one MCP tool as a types.RichTool.
type serverTool struct {
	client     *Client
	remote     string // the tool's name on the server, before prefixing
	def        types.ToolDef
	schema     json.RawMessage
	timeout    time.Duration
	server     string
	idempotent bool
}

var _ types.RichTool = (*serverTool)(nil)

func (t *serverTool) Definition() types.ToolDef { return t.def }

func (t *serverTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	res, err := t.ExecuteRich(ctx, args)
	return res.Text(), err
}

// ExecuteRich calls the tool and converts its content blocks. An MCP tool that
// reports an error returns it as a ToolResult with IsError set rather than a Go
// error: a failing tool is information for the model, not a broken agent.
func (t *serverTool) ExecuteRich(ctx context.Context, args map[string]any) (types.ToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	stop := context.AfterFunc(t.client.closing, cancel)
	defer stop()

	if !t.client.offers(t.remote) {
		return errorResult("mcp: server %q no longer offers tool %s", t.server, t.remote), nil
	}

	if sem := t.client.sem; sem != nil {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		case <-ctx.Done():
			return errorResult("mcp: call %s on %q waited for a free slot until %v", t.remote, t.server, ctx.Err()), nil
		}
	}

	if fn := t.client.spec.ArgTransform; fn != nil {
		args = fn(t.remote, t.schema, cloneArgs(args))
	}

	params := &mcpsdk.CallToolParams{Name: t.remote, Arguments: args}
	if handler := t.progressHandler(ctx); handler != nil {
		token := fmt.Sprintf("saige-%d", t.client.progressSeq.Add(1))
		t.client.progress.Store(token, progressEntry{tool: t.remote, fn: handler})
		defer t.client.progress.Delete(token)
		params.SetProgressToken(token)
	}

	session, err := t.client.beginCall()
	if err != nil {
		return types.ToolResult{Parts: []types.ToolOutputPart{types.Text(err.Error())}, IsError: true}, nil
	}
	defer t.client.calls.Done()

	res, err := session.CallTool(ctx, params)
	if err != nil && isDeadSession(err) && ctx.Err() == nil {
		// The session is gone, not the tool. Reconnect so later calls work,
		// and repeat this one only when doing so cannot apply it twice.
		fresh, rerr := t.client.reconnect(ctx, session)
		if rerr != nil {
			return errorResult("mcp: call %s on %q failed: %v; reconnect failed: %v", t.remote, t.server, err, rerr), nil
		}
		if !t.idempotent {
			return errorResult("mcp: server %q lost its session during %s and was reconnected; the call was not retried because the tool is not known to be idempotent and may have run", t.server, t.remote), nil
		}
		res, err = fresh.CallTool(ctx, params)
	}
	if err != nil {
		return errorResult("mcp: call %s on %q failed: %v", t.remote, t.server, err), nil
	}
	return t.convert(res), nil
}

func errorResult(format string, args ...any) types.ToolResult {
	return types.ToolResult{Parts: []types.ToolOutputPart{types.Text(fmt.Sprintf(format, args...))}, IsError: true}
}

// isDeadSession reports whether an error means the session itself is
// unusable. Application errors, including protocol-level tool errors, are
// deliberately excluded: evicting a healthy session on a routine refusal would
// cost every other caller a reconnect.
func isDeadSession(err error) bool {
	return errors.Is(err, mcpsdk.ErrConnectionClosed) || errors.Is(err, mcpsdk.ErrSessionMissing)
}

func cloneArgs(args map[string]any) map[string]any {
	out := make(map[string]any, len(args))
	for k, v := range args {
		out[k] = v
	}
	return out
}

// convert maps MCP content onto a ToolResult. Resource links become citations
// so an MCP-sourced fact lands in the same registry, with the same numbering,
// as one the provider found through its own web search.
//
// Content is bounded by the spec's result limits. The SDK has already decoded
// the response, so this bounds the transcript and journal footprint, not
// memory.
func (t *serverTool) convert(res *mcpsdk.CallToolResult) types.ToolResult {
	out := types.ToolResult{IsError: res.IsError}
	lim := newResultLimits(t.client.spec)
	hasText := false

	addText := func(s string) {
		out.Parts = append(out.Parts, types.Text(lim.text(s)))
		hasText = true
	}
	// addNote records dropped content as a text part, so the model learns
	// that something was left out.
	addNote := func(note string) {
		out.Parts = append(out.Parts, types.Text(note))
		hasText = true
	}
	addBinary := func(label, mime string, data []byte) {
		if !lim.binary(len(data)) {
			addNote(fmt.Sprintf("[dropped %s: %s, %d bytes]", label, mime, len(data)))
			return
		}
		src := types.Bytes(types.MediaType(mime), data)
		switch label {
		case "image":
			out.Parts = append(out.Parts, types.Image(src))
		case "audio":
			out.Parts = append(out.Parts, types.Audio(src))
		default:
			if p, ok := types.Media(src).(types.ToolOutputPart); ok {
				out.Parts = append(out.Parts, p)
			} else {
				out.Parts = append(out.Parts, types.File(src))
			}
		}
	}

	for _, content := range res.Content {
		switch c := content.(type) {
		case *mcpsdk.TextContent:
			addText(c.Text)
		case *mcpsdk.ImageContent:
			addBinary("image", c.MIMEType, c.Data)
		case *mcpsdk.AudioContent:
			addBinary("audio", c.MIMEType, c.Data)
		case *mcpsdk.ResourceLink:
			title := c.Title
			if title == "" {
				title = c.Name
			}
			cite := types.NewCitation(types.CitationTool, c.URI, title)
			cite.Producer = t.def.Name
			out.Citations = append(out.Citations, cite)
			out.Parts = append(out.Parts, types.Text("[resource: "+title+" "+c.URI+"]"))
			hasText = true
		case *mcpsdk.EmbeddedResource:
			if c.Resource == nil {
				continue
			}
			if c.Resource.Text != "" {
				addText(c.Resource.Text)
			}
			if len(c.Resource.Blob) > 0 {
				addBinary("resource", c.Resource.MIMEType, c.Resource.Blob)
			}
			if c.Resource.URI != "" {
				cite := types.NewCitation(types.CitationTool, c.Resource.URI, c.Resource.URI)
				cite.Producer = t.def.Name
				out.Citations = append(out.Citations, cite)
			}
		}
	}

	// StructuredContent is the machine-readable answer when the server provides
	// one; it is added as a JSON part so providers that accept structured tool
	// results get the real shape rather than a stringified copy. It is never
	// cut: a truncated JSON document is not JSON, so an oversized one is
	// dropped whole. When it doubles as the text projection it costs twice.
	if res.StructuredContent != nil {
		if raw, err := json.Marshal(res.StructuredContent); err == nil {
			cost := len(raw)
			if !hasText {
				cost *= 2
			}
			if lim.take(cost) {
				out.Parts = append(out.Parts, types.JSONPart{JSON: raw})
			} else {
				addNote(fmt.Sprintf("[dropped structured content: %d bytes over the result limit]", len(raw)))
			}
		}
	}

	if out.Text() == "" && !out.HasMedia() {
		out.Parts = append(out.Parts, types.Text("(no content)"))
	}
	return out
}

// ── Credential-injecting HTTP transport ─────────────────────────────

type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
	token   func(context.Context) (string, error)
}

func (h *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone before mutating: RoundTrip must not modify the caller's request.
	r := req.Clone(req.Context())
	for k, v := range h.headers {
		r.Header.Set(k, v)
	}
	if h.token != nil {
		tok, err := h.token(req.Context())
		if err != nil {
			// Never include the token source's state in the message beyond its
			// own error text.
			return nil, fmt.Errorf("mcp: resolve bearer token: %w", err)
		}
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	return h.base.RoundTrip(r)
}

// ── Schema conversion ───────────────────────────────────────────────

// schemaFromMCP converts an MCP input schema (free-form JSON) into a
// ParameterSchema. Unknown constructs are dropped rather than rejected: an
// approximate schema still lets the model call the tool, while a hard failure
// would make one odd tool break the whole server import.
func schemaFromMCP(raw any) types.ParameterSchema {
	m, ok := asMap(raw)
	if !ok {
		return types.ParameterSchema{Type: types.SchemaObject}
	}
	out := types.ParameterSchema{Type: types.SchemaObject, Properties: map[string]types.PropertyDef{}}
	if s, ok := m["type"].(string); ok && s != "" {
		out.Type = s
	}
	out.Required = stringSlice(m["required"])
	if props, ok := asMap(m["properties"]); ok {
		for name, p := range props {
			if pm, ok := asMap(p); ok {
				out.Properties[name] = propertyFromMCP(pm)
			}
		}
	}
	if len(out.Properties) == 0 {
		out.Properties = nil
	}
	return out
}

func propertyFromMCP(m map[string]any) types.PropertyDef {
	p := types.PropertyDef{Type: types.SchemaString}
	p.Nullable, _ = m["nullable"].(bool)
	if s, ok := m["type"].(string); ok && s != "" {
		p.Type = s
	} else if members := stringSlice(m["type"]); len(members) > 0 {
		// Preserve null regardless of its position. Other unions retain the
		// existing first-non-null approximation; this is not a general union API.
		base := ""
		for _, member := range members {
			if member == types.SchemaNull {
				p.Nullable = true
			} else if base == "" {
				base = member
			}
		}
		if base != "" {
			p.Type = base
		} else if p.Nullable {
			p.Type = types.SchemaNull
		}
	}
	if s, ok := m["description"].(string); ok {
		p.Description = s
	}
	p.Enum = stringSlice(m["enum"])
	p.Required = stringSlice(m["required"])
	if d, ok := m["default"]; ok {
		p.Default = d
	}
	if items, ok := asMap(m["items"]); ok {
		it := propertyFromMCP(items)
		p.Items = &it
	}
	if props, ok := asMap(m["properties"]); ok {
		p.Properties = map[string]types.PropertyDef{}
		for name, v := range props {
			if vm, ok := asMap(v); ok {
				p.Properties[name] = propertyFromMCP(vm)
			}
		}
	}
	return p
}

// asMap normalises the shapes an MCP schema can arrive in: a map, or raw JSON
// the SDK passed through unparsed.
func asMap(v any) (map[string]any, bool) {
	switch t := v.(type) {
	case map[string]any:
		return t, true
	case json.RawMessage:
		var m map[string]any
		if err := json.Unmarshal(t, &m); err == nil {
			return m, true
		}
	case []byte:
		var m map[string]any
		if err := json.Unmarshal(t, &m); err == nil {
			return m, true
		}
	}
	return nil, false
}

func stringSlice(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}
	return nil
}
