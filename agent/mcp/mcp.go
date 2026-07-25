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
//	failure mode     process exit, one shot        network, retryable
//	auth             environment variables         headers, bearer tokens
//	trust            code we chose to run          a third party
//	latency          microseconds                  a network round trip
//	tool list        stable per process            can change between calls
//
// The lifetime difference is the one that bites. A local server is a child
// process this package owns: failing to Close it leaks the process for the
// lifetime of the agent, and a crashed server does not come back, because
// there is nothing to reconnect to. A remote server needs none of that
// custody, but every call can fail transiently and its advertised tool list
// can change under you between connections, which is why AllowedTools and a
// gate matter far more there.
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
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

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
	// Headers are sent on every request, for bearer tokens and the like.
	Headers map[string]string
	// HTTPClient overrides the transport's client, for custom TLS or proxies.
	HTTPClient *http.Client

	// ── Common ──────────────────────────────────────────────────────

	// ToolPrefix is prepended to every imported tool name to keep two servers
	// exposing "search" from colliding. Empty defaults to Name + "_"; set it to
	// "-" to import names unchanged, which is only safe with one server.
	ToolPrefix string
	// AllowedTools restricts which of the server's tools are imported at all.
	// Empty imports everything advertised, which for a remote server means
	// trusting a list that can change between connections.
	AllowedTools []string
	// Gate is applied to this server's tools. Compose it into the agent's gate
	// via Pool.Gate. nil means no server-specific policy.
	Gate types.ToolGate
	// ConnectTimeout and CallTimeout bound the handshake and each call.
	ConnectTimeout time.Duration
	CallTimeout    time.Duration
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
	spec    ServerSpec
	session *mcpsdk.ClientSession
	cmd     *exec.Cmd // non-nil for local servers: the process we hold

	mu    sync.RWMutex
	tools map[string]*serverTool // by exposed (prefixed) name
}

// Connect dials the server and completes the MCP handshake. The caller owns
// the returned Client and must Close it.
func Connect(ctx context.Context, spec ServerSpec) (*Client, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}

	timeout := spec.ConnectTimeout
	if timeout <= 0 {
		timeout = DefaultConnectTimeout
	}
	connectCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "saige", Version: "1"}, nil)

	var transport mcpsdk.Transport
	var cmd *exec.Cmd
	if spec.IsLocal() {
		// The command must NOT take connectCtx: that context is cancelled when
		// the handshake finishes, which would kill the server we just started.
		cmd = exec.Command(spec.Command, spec.Args...)
		cmd.Env = spec.Env
		cmd.Dir = spec.WorkDir
		transport = &mcpsdk.CommandTransport{Command: cmd}
	} else {
		httpClient := spec.HTTPClient
		if len(spec.Headers) > 0 {
			httpClient = withHeaders(httpClient, spec.Headers)
		}
		transport = &mcpsdk.StreamableClientTransport{
			Endpoint:   spec.URL,
			HTTPClient: httpClient,
		}
	}

	session, err := client.Connect(connectCtx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: connect to %q: %w", spec.Name, err)
	}
	return &Client{spec: spec, session: session, cmd: cmd, tools: map[string]*serverTool{}}, nil
}

// Spec returns the spec this client was built from.
func (c *Client) Spec() ServerSpec { return c.spec }

// IsLocal reports whether this client holds a child process.
func (c *Client) IsLocal() bool { return c.cmd != nil }

// Tools lists the server's tools, filtered by AllowedTools, wrapped as
// types.Tool with prefixed names.
//
// The list is re-fetched on every call rather than cached at connect time,
// because a remote server may add or remove tools mid-session and a stale list
// produces "tool not found" errors from the model's point of view.
func (c *Client) Tools(ctx context.Context) ([]types.Tool, error) {
	res, err := c.session.ListTools(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: list tools on %q: %w", c.spec.Name, err)
	}

	allowed := map[string]bool{}
	for _, n := range c.spec.AllowedTools {
		allowed[n] = true
	}

	prefix := c.spec.prefix()
	out := make([]types.Tool, 0, len(res.Tools))

	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range res.Tools {
		if len(allowed) > 0 && !allowed[t.Name] {
			continue
		}
		st := &serverTool{
			client:   c,
			remote:   t.Name,
			def:      c.toolDef(prefix+t.Name, t),
			timeout:  c.spec.callTimeout(),
			server:   c.spec.Name,
			isRemote: !c.IsLocal(),
		}
		c.tools[st.def.Name] = st
		out = append(out, st)
	}
	return out, nil
}

// Register lists the server's tools and adds them to a registry, returning how
// many were added.
func (c *Client) Register(ctx context.Context, registry *types.ToolRegistry) (int, error) {
	tools, err := c.Tools(ctx)
	if err != nil {
		return 0, err
	}
	for _, t := range tools {
		registry.Register(t)
	}
	return len(tools), nil
}

// Close ends the session and, for a local server, waits for the child process
// to exit. Safe to call more than once.
func (c *Client) Close() error {
	if c.session == nil {
		return nil
	}
	err := c.session.Close()
	c.session = nil
	return err
}

// toolDef converts an MCP tool declaration into this SDK's ToolDef. The MCP
// input schema arrives as free-form JSON, so unconvertible schemas degrade to
// an empty object rather than failing the import: a tool with a schema we
// cannot read is still callable, just not well described.
func (c *Client) toolDef(name string, t *mcpsdk.Tool) types.ToolDef {
	desc := t.Description
	if desc == "" {
		desc = "MCP tool " + t.Name + " on server " + c.spec.Name
	}
	return types.ToolDef{
		Name:        name,
		Description: desc,
		Parameters:  schemaFromMCP(t.InputSchema),
	}
}

// ── Tool wrapper ────────────────────────────────────────────────────

// serverTool exposes one MCP tool as a types.RichTool.
type serverTool struct {
	client   *Client
	remote   string // the tool's name on the server, before prefixing
	def      types.ToolDef
	timeout  time.Duration
	server   string
	isRemote bool
}

var _ types.RichTool = (*serverTool)(nil)

func (t *serverTool) Definition() types.ToolDef { return t.def }

func (t *serverTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	res, err := t.ExecuteRich(ctx, args)
	return res.Text, err
}

// ExecuteRich calls the tool and converts its content blocks. An MCP tool that
// reports an error returns it as a ToolResult with IsError set rather than a Go
// error: a failing tool is information for the model, not a broken agent.
func (t *serverTool) ExecuteRich(ctx context.Context, args map[string]any) (types.ToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	res, err := t.client.session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      t.remote,
		Arguments: args,
	})
	if err != nil {
		return types.ToolResult{
			Text:    fmt.Sprintf("mcp: call %s on %q failed: %v", t.remote, t.server, err),
			IsError: true,
		}, nil
	}
	return t.convert(res), nil
}

// convert maps MCP content onto a ToolResult. Resource links become citations
// so an MCP-sourced fact lands in the same registry, with the same numbering,
// as one the provider found through its own web search.
func (t *serverTool) convert(res *mcpsdk.CallToolResult) types.ToolResult {
	out := types.ToolResult{IsError: res.IsError}
	var text []string

	for _, content := range res.Content {
		switch c := content.(type) {
		case *mcpsdk.TextContent:
			text = append(text, c.Text)
			out.Blocks = append(out.Blocks, types.ToolResultBlock{
				Kind: types.ToolResultBlockText, Text: c.Text,
			})
		case *mcpsdk.ImageContent:
			out.Blocks = append(out.Blocks, types.ToolResultBlock{
				Kind:      types.ToolResultBlockImage,
				MediaType: types.MediaType(c.MIMEType),
				Data:      c.Data,
			})
			text = append(text, "[image: "+c.MIMEType+"]")
		case *mcpsdk.AudioContent:
			out.Blocks = append(out.Blocks, types.ToolResultBlock{
				Kind:      types.ToolResultBlockFile,
				MediaType: types.MediaType(c.MIMEType),
				Data:      c.Data,
			})
			text = append(text, "[audio: "+c.MIMEType+"]")
		case *mcpsdk.ResourceLink:
			title := c.Title
			if title == "" {
				title = c.Name
			}
			cite := types.NewCitation(types.CitationTool, c.URI, title)
			cite.Producer = t.def.Name
			out.Citations = append(out.Citations, cite)
			text = append(text, "[resource: "+title+" "+c.URI+"]")
		case *mcpsdk.EmbeddedResource:
			if c.Resource == nil {
				continue
			}
			if c.Resource.Text != "" {
				text = append(text, c.Resource.Text)
				out.Blocks = append(out.Blocks, types.ToolResultBlock{
					Kind: types.ToolResultBlockText, Text: c.Resource.Text,
				})
			}
			if c.Resource.URI != "" {
				cite := types.NewCitation(types.CitationTool, c.Resource.URI, c.Resource.URI)
				cite.Producer = t.def.Name
				out.Citations = append(out.Citations, cite)
			}
		}
	}

	// StructuredContent is the machine-readable answer when the server provides
	// one; it is added as a JSON block so providers that accept structured tool
	// results get the real shape rather than a stringified copy.
	if res.StructuredContent != nil {
		if raw, err := json.Marshal(res.StructuredContent); err == nil {
			out.Blocks = append(out.Blocks, types.ToolResultBlock{
				Kind: types.ToolResultBlockJSON, JSON: raw,
			})
			if len(text) == 0 {
				text = append(text, string(raw))
			}
		}
	}

	out.Text = strings.Join(text, "\n")
	if out.Text == "" {
		out.Text = "(no content)"
	}
	return out
}

// ── Header-injecting HTTP transport ─────────────────────────────────

type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (h *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone before mutating: RoundTrip must not modify the caller's request.
	r := req.Clone(req.Context())
	for k, v := range h.headers {
		r.Header.Set(k, v)
	}
	return h.base.RoundTrip(r)
}

func withHeaders(client *http.Client, headers map[string]string) *http.Client {
	base := http.DefaultTransport
	out := &http.Client{}
	if client != nil {
		*out = *client
		if client.Transport != nil {
			base = client.Transport
		}
	}
	out.Transport = &headerTransport{base: base, headers: headers}
	return out
}

// ── Schema conversion ───────────────────────────────────────────────

// schemaFromMCP converts an MCP input schema (free-form JSON) into a
// ParameterSchema. Unknown constructs are dropped rather than rejected: an
// approximate schema still lets the model call the tool, while a hard failure
// would make one odd tool break the whole server import.
func schemaFromMCP(raw any) types.ParameterSchema {
	m, ok := asMap(raw)
	if !ok {
		return types.ParameterSchema{Type: "object"}
	}
	out := types.ParameterSchema{Type: "object", Properties: map[string]types.PropertyDef{}}
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
	p := types.PropertyDef{Type: "string"}
	if s, ok := m["type"].(string); ok && s != "" {
		p.Type = s
	} else if list, ok := m["type"].([]any); ok {
		// JSON Schema allows a type union; take the first non-null member,
		// which is the one the model should generate.
		for _, v := range list {
			if s, ok := v.(string); ok && s != "null" {
				p.Type = s
				break
			}
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
