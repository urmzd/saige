package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/definition"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/cmd/internal/agenthost"
	"github.com/urmzd/saige/tools"
	"github.com/urmzd/saige/tools/exec"
	"github.com/urmzd/saige/tools/fetch"
	"github.com/urmzd/saige/tools/fs"
)

func newServeCmd(ctx context.Context) *cobra.Command {
	var (
		addr            string
		token           string
		approvalTimeout time.Duration
		idleTTL         time.Duration
		packs           []string
		workspace       string
		bashNetwork     string
		denyAfter       int
		agentRef        string
		agentsReload    time.Duration
		maxUpload       int64
		artifactBudget  int64
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve the agent over HTTP with a Server-Sent Events turn stream",
		Long: `Serve the agent over HTTP. Each session owns a conversation; each turn
streams its events as Server-Sent Events whose data is the versioned wire
envelope. Approvals and cancellation are POST endpoints.

  POST /v1/sessions                                         -> {session_id}
  POST /v1/sessions/{sid}/artifacts          <bytes>        -> 201 {ref, sha256, size, media_type, url}
  GET  /v1/sessions/{sid}/artifacts/{sha256}                -> the bytes, as an attachment
  POST /v1/sessions/{sid}/turns              {parts}        -> 202 {turn_id}
  GET  /v1/sessions/{sid}/turns/{tid}/events                -> SSE (Last-Event-ID resumes)
  GET  /v1/sessions/{sid}/turns/{tid}/events?wire=1         -> SSE in wire version 1
  GET  /v1/sessions/{sid}/turns/{tid}/events?format=agui    -> SSE as AG-UI events
  POST /v1/sessions/{sid}/turns/{tid}/interrupts/{call_id}  {approved, message, modified_args, grant}
  POST /v1/sessions/{sid}/turns/{tid}/cancel
  GET  /v1/sessions/{sid}/turns/{tid}                       -> {done, last_seq, error}
  GET  /v1/sessions/{sid}/tree                              -> conversation tree JSON

A turn's parts use the shared part codec, for example:

  {"parts": [{"type": "text", "text": "What is in this image?"},
             {"type": "image", "source": {"media_type": "image/png", "ref": "saige-artifact://<sha256>"}}]}

Media is sent as inline data (up to 256 KiB per part), an https URI, or the
ref of an upload to the session. Vendor file IDs are refused. The older
{"message": "<text>"} body still works and is deprecated.

Events use wire version 2 unless the client asks for version 1 with
?wire=1 or "Accept: application/vnd.saige.events+json;v=1". Media over the
inline limit in a run's output is stored in the session's artifacts and sent
as its ref.

The server binds to localhost by default. Binding to another address
requires a bearer token (--token or SAIGE_SERVE_TOKEN). An approval nobody
answers within --approval-timeout is denied.

An approval may carry a grant, so later calls it covers run without asking
for the rest of the session:

  {"approved": true, "grant": {"scope": "tool"}}
  {"approved": true, "grant": {"scope": "args", "match": [{"field": "path", "path_prefix": "/srv/app"}],
                               "expires_at": "2026-12-31T00:00:00Z"}}

Scopes are once, tool, args and session. Grants never cover destructive
tools. --deny-after stops asking about a tool after that many denials.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cf := persistentFlagVars
			if token == "" {
				token = os.Getenv("SAIGE_SERVE_TOKEN")
			}
			if err := checkServeAddr(addr, token); err != nil {
				return err
			}
			srvCtx, stop := context.WithCancel(ctx)
			defer stop()

			opts := serveOptions{token: token, approvalTimeout: approvalTimeout, idleTTL: idleTTL,
				maxUpload: maxUpload, artifactBudget: artifactBudget}
			if agentRef != "" {
				h, err := newAgentHost(ctx, cmd, cf, tools.HarnessOptions{Root: workspace, Network: exec.NetworkPolicy(bashNetwork)}, false)
				if err != nil {
					return err
				}
				defer h.cleanup()
				// Bind once now, so a definition this host cannot serve
				// fails at start rather than on the first session.
				first, err := h.bind(ctx, agentRef)
				if err != nil {
					return err
				}
				slog.Info("saige serve agent", "agent", first.Pin().String())
				_ = first.Close(ctx)
				if agentsReload > 0 {
					stopWatch, err := h.reg.Watch(srvCtx, definition.WatchOptions{Interval: agentsReload, OnReload: func(changed bool, err error) {
						switch {
						case err != nil:
							slog.Warn("saige serve: agent definitions reload failed; keeping the previous set", "error", err)
						case changed:
							slog.Info("saige serve: agent definitions reloaded; new sessions use them")
						}
					}})
					if err != nil {
						return err
					}
					defer stopWatch()
				}
				// Each session binds the definition the registry resolves
				// when it starts and keeps it, pinned, until it ends.
				opts.newSessionAgent = func() (agenthost.Agent, error) {
					b, err := h.bind(srvCtx, agentRef)
					if err != nil {
						return agenthost.Agent{}, err
					}
					return agenthost.FromBound(b), nil
				}
				return listenAndServe(ctx, srvCtx, cmd, addr, opts, -1)
			}

			provider, err := resolveProvider(ctx, cf, false)
			if err != nil {
				return err
			}
			tools, cleanup, err := buildTools(ctx, cf)
			if err != nil {
				return err
			}
			defer cleanup()
			packTools, err := buildPackTools(packs, workspace, bashNetwork)
			if err != nil {
				return err
			}
			tools = append(tools, packTools...)

			opts.newAgent = func() (*agentsdk.Agent, error) {
				cfg := agentsdk.AgentConfig{Name: cliName, SystemPrompt: *cf.system, Provider: provider}
				if len(tools) > 0 {
					cfg.Tools = types.NewToolRegistry(tools...)
				}
				return agentsdk.NewAgent(cfg, agentsdk.WithApprovalPolicy(agentsdk.ApprovalPolicy{DenyAfter: denyAfter})), nil
			}
			return listenAndServe(ctx, srvCtx, cmd, addr, opts, len(tools))
		},
	}
	f := cmd.Flags()
	f.StringVar(&addr, "addr", "127.0.0.1:8787", "Listen address")
	f.StringVar(&token, "token", "", "Bearer token required on every request (default $SAIGE_SERVE_TOKEN)")
	f.DurationVar(&approvalTimeout, "approval-timeout", 10*time.Minute, "Deny an approval that gets no decision within this time")
	f.DurationVar(&idleTTL, "idle-ttl", time.Hour, "Drop a session that has had no running turn for this long")
	f.StringSliceVar(&packs, "tools", nil, "Tool packs to enable: fs, fs-write, fetch, bash")
	f.StringVar(&workspace, "workspace", "", "Workspace root for the fs and bash packs")
	f.IntVar(&denyAfter, "deny-after", 0, "Refuse a tool without asking after this many denials in a session (0 always asks)")
	f.StringVar(&bashNetwork, "bash-network", string(exec.NetworkDeny), "Network policy for bash: deny (needs an isolating wrapper) or allow")
	f.Int64Var(&maxUpload, "max-upload", defaultMaxUpload, "Largest artifact upload, in bytes")
	f.Int64Var(&artifactBudget, "artifact-budget", 256<<20, "Bytes of artifacts one session may hold")
	f.StringVar(&agentRef, "agent", "", "Serve an agent definition: NAME or NAME@RANGE; each session pins the version it starts with")
	f.DurationVar(&agentsReload, "agents-reload", 0, "With --agent, reload the definitions this often so new sessions see changes (0 never reloads)")
	return cmd
}

// listenAndServe runs the HTTP server until ctx ends. tools is logged; a
// negative count is not.
func listenAndServe(ctx, srvCtx context.Context, cmd *cobra.Command, addr string, opts serveOptions, tools int) error {
	s := newServer(srvCtx, opts)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	httpSrv := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()
	if tools >= 0 {
		slog.Info("saige serve listening", "addr", ln.Addr().String(), "tools", tools)
	} else {
		slog.Info("saige serve listening", "addr", ln.Addr().String())
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "listening on http://%s\n", ln.Addr())
	if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// checkServeAddr refuses to expose the server beyond this machine without a
// token.
func checkServeAddr(addr, token string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid --addr %q: %w", addr, err)
	}
	if token != "" || isLoopbackHost(host) {
		return nil
	}
	return fmt.Errorf("--addr %s is not a loopback address; set --token or SAIGE_SERVE_TOKEN to serve beyond localhost", addr)
}

// buildPackTools builds the opt-in tool packs named in packs. The fs and
// bash packs need a workspace root. Mutating tools carry approval markers,
// which the serve API resolves through its interrupt endpoint.
func buildPackTools(packs []string, workspace, bashNetwork string) ([]types.Tool, error) {
	want := map[string]bool{}
	for _, p := range packs {
		p = strings.TrimSpace(p)
		switch p {
		case "":
			continue
		case "fs", "fs-write", "fetch", "bash":
			want[p] = true
		default:
			return nil, fmt.Errorf("unknown tool pack %q (want fs, fs-write, fetch, or bash)", p)
		}
	}
	if (want["fs"] || want["fs-write"] || want["bash"]) && workspace == "" {
		return nil, errors.New("--workspace is required for the fs and bash packs")
	}

	var tools []types.Tool
	if want["fs"] || want["fs-write"] {
		var opts []fs.Option
		if want["fs-write"] {
			opts = append(opts, fs.AllowWrites())
		}
		ts, err := fs.NewTools(workspace, opts...)
		if err != nil {
			return nil, err
		}
		tools = append(tools, ts...)
	}
	if want["fetch"] {
		tools = append(tools, fetch.NewTool())
	}
	if want["bash"] {
		policy := exec.DefaultPolicy()
		policy.Network = exec.NetworkPolicy(bashNetwork)
		var sbOpts []exec.SubprocessOption
		switch policy.Network {
		case exec.NetworkDeny:
			sbOpts = append(sbOpts, exec.NoNetwork())
		case exec.NetworkAllow:
		default:
			return nil, fmt.Errorf("--bash-network must be deny or allow, got %q", bashNetwork)
		}
		sb, err := exec.NewSubprocess(sbOpts...)
		if err != nil {
			return nil, fmt.Errorf("bash pack: %w", err)
		}
		bash, err := exec.NewBashTool(sb, workspace, policy)
		if err != nil {
			return nil, err
		}
		tools = append(tools, bash)
	}
	return tools, nil
}
