package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/spf13/cobra"

	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/definition/bind"
	"github.com/urmzd/saige/agent/mcp"
	"github.com/urmzd/saige/agent/provider"
	"github.com/urmzd/saige/agent/provider/preset"
	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/cmd/internal/agenthost"
	"github.com/urmzd/saige/tools"
	"github.com/urmzd/saige/tools/exec"
)

// Session configuration option IDs.
const (
	acpConfigAgent = "agent"
	acpConfigModel = "model"
	// acpModelDefault keeps the model the definition names.
	acpModelDefault = "default"
	// acpAuthEnv is the one authentication method: credentials from the
	// environment saige acp was started with.
	acpAuthEnv = "env"
)

func newACPCmd(ctx context.Context) *cobra.Command {
	var (
		agentRef        string
		workspace       string
		sessionsDir     string
		approvalTimeout time.Duration
		network         string
	)
	cmd := &cobra.Command{
		Use:   "acp",
		Short: "Run an agent definition as an Agent Client Protocol agent over stdio",
		Long: `Run an agent definition as an ACP agent (protocol version 1) on stdin and
stdout, for editors that host external agents: Zed, JetBrains, Neovim and
Emacs clients, and others. Logs go to stderr.

Each ACP session binds the definition when it starts and keeps that pinned
version. The session's working directory is the workspace of the
definition's harness tools unless --workspace sets one. MCP servers the
client forwards are connected only when the definition names them under
tools.mcp. Approvals become session/request_permission; "always allow"
grants the tool for the session, within the definition's approval.grant.
The agent and the model are session config options.

With --sessions-dir, conversations are saved after every turn and can be
loaded again (session/load) or listed (session/list).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if agentRef == "" {
				return invalidInput(errors.New("--agent is required"))
			}
			cf := persistentFlagVars
			h, err := newAgentHost(ctx, cmd, cf, tools.HarnessOptions{Network: exec.NetworkPolicy(network)}, false)
			if err != nil {
				return err
			}
			defer h.cleanup()
			// Bind once now, so a definition this host cannot serve fails at
			// start rather than on the first session.
			cwd, _ := os.Getwd()
			first, err := h.bindWith(ctx, agentRef, func(env *bind.Env) error {
				env.Harness.Root = firstNonEmpty(workspace, cwd)
				return nil
			})
			if err != nil {
				return err
			}
			slog.Info("saige acp agent", "agent", first.Pin().String())
			_ = first.Close(ctx)

			srv := newACPServer(ctx, acpOptions{
				agent:           agentRef,
				agents:          func() []string { return definitionNames(h) },
				models:          presetNames(h),
				bind:            acpBinder(h, workspace),
				workspace:       workspace,
				sessionsDir:     sessionsDir,
				approvalTimeout: approvalTimeout,
			})
			conn := acp.NewAgentSideConnection(srv, os.Stdout, os.Stdin)
			conn.SetLogger(slog.New(slog.NewTextHandler(os.Stderr, nil)))
			srv.client = conn
			select {
			case <-conn.Done():
			case <-ctx.Done():
			}
			srv.close()
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&agentRef, "agent", "", "Agent definition to run: NAME or NAME@RANGE (required)")
	f.StringVar(&workspace, "workspace", "", "Workspace root for harness tools (default: each session's working directory)")
	f.StringVar(&sessionsDir, "sessions-dir", "", "Save conversations here so clients can load them again (default: not saved)")
	f.DurationVar(&approvalTimeout, "approval-timeout", 10*time.Minute, "Deny a permission request that gets no answer within this time")
	f.StringVar(&network, "exec-network", string(exec.NetworkDeny), "Network policy for execute_code: deny (needs an isolating sandbox) or allow")
	return cmd
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// definitionNames lists the names of every definition the host can bind.
func definitionNames(h *agentHost) []string {
	var names []string
	for _, d := range h.reg.List() {
		if !slices.Contains(names, d.Name) {
			names = append(names, d.Name)
		}
	}
	slices.Sort(names)
	return names
}

// presetNames lists the catalog's presets, the models a session may pick.
func presetNames(h *agentHost) []string {
	var out []string
	for _, n := range h.env.Catalog.PresetNames() {
		out = append(out, string(n))
	}
	return out
}

// acpBinder binds a definition for one ACP session: the workspace is the
// session's directory unless one was set, the client's MCP servers join
// the host's (a host server of the same name wins), and a model other than
// the default replaces the definition's.
func acpBinder(h *agentHost, workspace string) acpBindFunc {
	return func(ctx context.Context, req acpBindRequest) (agenthost.Agent, error) {
		b, err := h.bindWith(ctx, req.agent, func(env *bind.Env) error {
			env.Harness.Root = firstNonEmpty(workspace, req.cwd)
			if len(req.servers) > 0 {
				merged := maps.Clone(env.MCPServers)
				if merged == nil {
					merged = map[string]mcp.ServerSpec{}
				}
				for _, s := range req.servers {
					if _, taken := merged[s.Name]; taken {
						slog.Warn("saige acp: the client's MCP server is shadowed by the host's", "server", s.Name)
						continue
					}
					merged[s.Name] = s
				}
				env.MCPServers = merged
			}
			if req.model != "" && req.model != acpModelDefault {
				p, err := preset.Build(ctx, env.Catalog, types.PresetName(req.model), nil, env.PresetOptions)
				if err != nil {
					return fmt.Errorf("model %s: %w", req.model, err)
				}
				env.Preset = p
			}
			return nil
		})
		if err != nil {
			return agenthost.Agent{}, err
		}
		var opts []agentsdk.AgentOption
		if req.tree != nil {
			opts = append(opts, agentsdk.WithTree(req.tree))
		}
		return agenthost.FromBound(b, opts...), nil
	}
}

// acpBindRequest is what a session binds its agent from.
type acpBindRequest struct {
	agent, model, cwd string
	servers           []mcp.ServerSpec
	// tree, when set, is the conversation the agent continues.
	tree *tree.Tree
}

type acpBindFunc func(context.Context, acpBindRequest) (agenthost.Agent, error)

// acpClient is the part of the client connection the server calls.
type acpClient interface {
	SessionUpdate(ctx context.Context, params acp.SessionNotification) error
	RequestPermission(ctx context.Context, params acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error)
}

type acpOptions struct {
	// agent is the definition new sessions start with.
	agent string
	// agents lists the definitions a session may switch to; models the
	// presets it may pick.
	agents func() []string
	models []string
	bind   acpBindFunc
	// workspace, when set, is every session's harness root instead of its
	// working directory.
	workspace string
	// sessionsDir, when set, saves conversations for session/load.
	sessionsDir     string
	approvalTimeout time.Duration
	maxSessions     int
}

// acpServer implements acp.Agent on the shared session layer.
type acpServer struct {
	ctx      context.Context
	cancel   context.CancelFunc
	opts     acpOptions
	client   acpClient
	sessions *agenthost.Manager[*acpSession]
}

// acpSession is an ACP session's own state.
type acpSession struct {
	cwd     string
	servers []mcp.ServerSpec
	// agent and model are the session's config option values.
	agent, model string
}

var (
	_ acp.Agent       = (*acpServer)(nil)
	_ acp.AgentLoader = (*acpServer)(nil)
)

func newACPServer(ctx context.Context, opts acpOptions) *acpServer {
	if opts.approvalTimeout <= 0 {
		opts.approvalTimeout = 10 * time.Minute
	}
	ctx, cancel := context.WithCancel(ctx)
	return &acpServer{
		ctx: ctx, cancel: cancel, opts: opts,
		// The client owns a session's lifetime: it is never dropped for
		// being idle while the connection lasts.
		sessions: agenthost.NewManager[*acpSession](agenthost.Options{Max: opts.maxSessions, Prefix: "sess_", IdleTTL: 100 * 365 * 24 * time.Hour}),
	}
}

// close ends every session.
func (s *acpServer) close() {
	s.cancel()
	s.sessions.CloseAll()
}

// Initialize negotiates protocol version 1 and advertises what saige
// accepts: images, audio and embedded resources in prompts, HTTP MCP
// servers, closing sessions, and with a sessions directory loading and
// listing them.
func (s *acpServer) Initialize(_ context.Context, _ acp.InitializeRequest) (acp.InitializeResponse, error) {
	caps := acp.AgentCapabilities{
		PromptCapabilities:  acp.PromptCapabilities{Image: true, Audio: true, EmbeddedContext: true},
		McpCapabilities:     acp.McpCapabilities{Http: true},
		SessionCapabilities: acp.SessionCapabilities{Close: &acp.SessionCloseCapabilities{}, Resume: &acp.SessionResumeCapabilities{}},
	}
	if s.opts.sessionsDir != "" {
		caps.LoadSession = true
		caps.SessionCapabilities.List = &acp.SessionListCapabilities{}
	}
	return acp.InitializeResponse{
		ProtocolVersion:   acp.ProtocolVersionNumber,
		AgentCapabilities: caps,
		AgentInfo:         &acp.Implementation{Name: cliName, Title: acp.Ptr("saige"), Version: version},
		AuthMethods: []acp.AuthMethod{{Agent: &acp.AuthMethodAgent{
			Id:   acpAuthEnv,
			Name: "Provider credentials from the environment",
			Description: acp.Ptr("saige reads model provider credentials from the environment it is started with, such as " +
				"ANTHROPIC_API_KEY, OPENAI_API_KEY, GOOGLE_API_KEY, or GOOGLE_GENAI_USE_VERTEXAI with Application Default Credentials."),
		}}},
	}, nil
}

// Authenticate succeeds when the environment holds any provider
// credentials; saige has no login of its own.
func (s *acpServer) Authenticate(_ context.Context, req acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	if req.MethodId != acpAuthEnv {
		return acp.AuthenticateResponse{}, acp.NewInvalidParams(map[string]any{"error": "unknown auth method " + req.MethodId})
	}
	for _, envs := range provider.APIKeyEnv {
		for _, e := range envs {
			if os.Getenv(e) != "" {
				return acp.AuthenticateResponse{}, nil
			}
		}
	}
	if provider.VertexEnabled(os.Getenv) || os.Getenv("OLLAMA_HOST") != "" {
		return acp.AuthenticateResponse{}, nil
	}
	return acp.AuthenticateResponse{}, acp.NewAuthRequired(map[string]any{
		"error": "no model provider credentials in the environment; set ANTHROPIC_API_KEY, OPENAI_API_KEY or GOOGLE_API_KEY where the client starts saige acp",
	})
}

func (s *acpServer) Logout(context.Context, acp.LogoutRequest) (acp.LogoutResponse, error) {
	return acp.LogoutResponse{}, acp.NewMethodNotFound(acp.AgentMethodLogout)
}

func (s *acpServer) SetSessionMode(context.Context, acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	// Modes are deprecated; the agent and model are config options.
	return acp.SetSessionModeResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionSetMode)
}

// NewSession binds the session's agent: the starting definition, the
// client's MCP servers it names, and the session's directory as workspace.
func (s *acpServer) NewSession(ctx context.Context, req acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	st, err := newACPSessionState(req.Cwd, req.McpServers, s.opts.agent)
	if err != nil {
		return acp.NewSessionResponse{}, err
	}
	sess, err := s.sessions.Create("", st, func() (agenthost.Agent, error) {
		return s.opts.bind(ctx, acpBindRequest{agent: st.agent, cwd: st.cwd, servers: st.servers})
	})
	if err != nil {
		return acp.NewSessionResponse{}, toACPError(err)
	}
	return acp.NewSessionResponse{SessionId: acp.SessionId(sess.ID), ConfigOptions: s.configOptions(st)}, nil
}

func newACPSessionState(cwd string, servers []acp.McpServer, agent string) (*acpSession, error) {
	if !filepath.IsAbs(cwd) {
		return nil, acp.NewInvalidParams(map[string]any{"error": "cwd must be an absolute path"})
	}
	specs, err := acpMCPServers(servers)
	if err != nil {
		return nil, err
	}
	return &acpSession{cwd: cwd, servers: specs, agent: agent, model: acpModelDefault}, nil
}

// acpMCPServers converts the servers a client forwards. SSE servers are
// skipped: saige does not advertise them.
func acpMCPServers(servers []acp.McpServer) ([]mcp.ServerSpec, error) {
	var out []mcp.ServerSpec
	for _, srv := range servers {
		switch {
		case srv.Stdio != nil:
			spec := mcp.ServerSpec{Name: srv.Stdio.Name, Command: srv.Stdio.Command, Args: srv.Stdio.Args}
			if len(srv.Stdio.Env) > 0 {
				// An explicit environment replaces the inherited one, so
				// add the client's variables to this process's.
				spec.Env = os.Environ()
				for _, e := range srv.Stdio.Env {
					spec.Env = append(spec.Env, e.Name+"="+e.Value)
				}
			}
			out = append(out, spec)
		case srv.Http != nil:
			spec := mcp.ServerSpec{Name: srv.Http.Name, URL: srv.Http.Url}
			if len(srv.Http.Headers) > 0 {
				spec.Headers = map[string]string{}
				for _, h := range srv.Http.Headers {
					spec.Headers[h.Name] = h.Value
				}
			}
			out = append(out, spec)
		case srv.Sse != nil:
			slog.Warn("saige acp: skipping an SSE MCP server; saige connects stdio and streamable HTTP servers", "server", srv.Sse.Name)
		}
	}
	return out, nil
}

// configOptions describes the session's agent and model selectors.
func (s *acpServer) configOptions(st *acpSession) []acp.SessionConfigOption {
	agents := s.opts.agents()
	if !slices.Contains(agents, st.agent) {
		agents = append([]string{st.agent}, agents...)
	}
	agentOpts := make(acp.SessionConfigSelectOptionsUngrouped, 0, len(agents))
	for _, a := range agents {
		agentOpts = append(agentOpts, acp.SessionConfigSelectOption{Name: a, Value: acp.SessionConfigValueId(a)})
	}
	modelOpts := acp.SessionConfigSelectOptionsUngrouped{{
		Name: "Definition default", Value: acpModelDefault,
		Description: acp.Ptr("The model the agent definition names"),
	}}
	for _, m := range s.opts.models {
		modelOpts = append(modelOpts, acp.SessionConfigSelectOption{Name: m, Value: acp.SessionConfigValueId(m)})
	}
	return []acp.SessionConfigOption{
		{Select: &acp.SessionConfigOptionSelect{
			Id: acpConfigAgent, Name: "Agent", Type: "select",
			Description:  acp.Ptr("The saige agent definition; switching starts a new conversation"),
			CurrentValue: acp.SessionConfigValueId(st.agent),
			Options:      acp.SessionConfigSelectOptions{Ungrouped: &agentOpts},
		}},
		{Select: &acp.SessionConfigOptionSelect{
			Id: acpConfigModel, Name: "Model", Type: "select", Category: acp.Ptr(acp.SessionConfigOptionCategoryModel),
			Description:  acp.Ptr("A catalog preset, or the definition's own model; switching keeps the conversation"),
			CurrentValue: acp.SessionConfigValueId(st.model),
			Options:      acp.SessionConfigSelectOptions{Ungrouped: &modelOpts},
		}},
	}
}

// SetSessionConfigOption rebinds the session between turns. A new model
// keeps the conversation; a new agent starts a fresh one, since the
// conversation carries the old agent's system prompt.
func (s *acpServer) SetSessionConfigOption(ctx context.Context, req acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	if req.ValueId == nil {
		return acp.SetSessionConfigOptionResponse{}, acp.NewInvalidParams(map[string]any{"error": "saige has no boolean config options"})
	}
	v := req.ValueId
	sess := s.sessions.Get(string(v.SessionId))
	if sess == nil {
		return acp.SetSessionConfigOptionResponse{}, acp.NewInvalidParams(map[string]any{"error": "unknown session " + string(v.SessionId)})
	}
	st := sess.Host
	next := acpBindRequest{agent: st.agent, model: st.model, cwd: st.cwd, servers: st.servers}
	value := string(v.Value)
	switch string(v.ConfigId) {
	case acpConfigAgent:
		next.agent, next.model = value, acpModelDefault
	case acpConfigModel:
		if value != acpModelDefault && !slices.Contains(s.opts.models, value) {
			return acp.SetSessionConfigOptionResponse{}, acp.NewInvalidParams(map[string]any{"error": "unknown model " + value})
		}
		next.model = value
		next.tree = sess.Agent().Agent.Tree()
	default:
		return acp.SetSessionConfigOptionResponse{}, acp.NewInvalidParams(map[string]any{"error": "unknown config option " + string(v.ConfigId)})
	}
	if sess.Running() {
		return acp.SetSessionConfigOptionResponse{}, toACPError(agenthost.ErrBusy)
	}
	ag, err := s.opts.bind(ctx, next)
	if err != nil {
		return acp.SetSessionConfigOptionResponse{}, toACPError(err)
	}
	if err := sess.Replace(ag); err != nil {
		return acp.SetSessionConfigOptionResponse{}, toACPError(err)
	}
	st.agent, st.model = next.agent, next.model
	return acp.SetSessionConfigOptionResponse{ConfigOptions: s.configOptions(st)}, nil
}

// Cancel stops the session's running turn. The SDK also cancels the
// prompt's context, which ends Prompt with the cancelled stop reason.
func (s *acpServer) Cancel(_ context.Context, req acp.CancelNotification) error {
	if sess := s.sessions.Get(string(req.SessionId)); sess != nil {
		sess.Cancel()
	}
	return nil
}

func (s *acpServer) CloseSession(_ context.Context, req acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	if s.sessions.Remove(string(req.SessionId)) == nil {
		return acp.CloseSessionResponse{}, acp.NewInvalidParams(map[string]any{"error": "unknown session " + string(req.SessionId)})
	}
	return acp.CloseSessionResponse{}, nil
}

// toACPError maps session errors to JSON-RPC errors a client can show.
func toACPError(err error) error {
	var re *acp.RequestError
	if errors.As(err, &re) {
		return err
	}
	switch {
	case errors.Is(err, agenthost.ErrBusy), errors.Is(err, agenthost.ErrLimit), errors.Is(err, agenthost.ErrClosed):
		return acp.NewInvalidRequest(map[string]any{"error": err.Error()})
	}
	return acp.NewInternalError(map[string]any{"error": err.Error()})
}
