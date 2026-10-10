package main

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"

	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/cmd/internal/agenthost"
)

// acpTestClient is an ACP client that records updates and answers
// permission requests with a fixed option.
type acpTestClient struct {
	mu          sync.Mutex
	updates     []acp.SessionUpdate
	permissions []acp.RequestPermissionRequest
	// answer is the option id to select; "" leaves the request unanswered
	// until its context ends.
	answer string
	onTool func(acp.SessionUpdate)
}

func (c *acpTestClient) SessionUpdate(_ context.Context, n acp.SessionNotification) error {
	c.mu.Lock()
	c.updates = append(c.updates, n.Update)
	onTool := c.onTool
	c.mu.Unlock()
	if onTool != nil && n.Update.ToolCallUpdate != nil {
		onTool(n.Update)
	}
	return nil
}

func (c *acpTestClient) RequestPermission(ctx context.Context, req acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	c.mu.Lock()
	c.permissions = append(c.permissions, req)
	answer := c.answer
	c.mu.Unlock()
	if answer == "" {
		<-ctx.Done()
		return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}, nil
	}
	return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected(acp.PermissionOptionId(answer))}, nil
}

func (c *acpTestClient) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, errors.New("not supported")
}

func (c *acpTestClient) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, errors.New("not supported")
}

func (c *acpTestClient) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, errors.New("not supported")
}

func (c *acpTestClient) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, errors.New("not supported")
}

func (c *acpTestClient) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, errors.New("not supported")
}

func (c *acpTestClient) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, errors.New("not supported")
}

func (c *acpTestClient) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, errors.New("not supported")
}

func (c *acpTestClient) snapshot() ([]acp.SessionUpdate, []acp.RequestPermissionRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]acp.SessionUpdate(nil), c.updates...), append([]acp.RequestPermissionRequest(nil), c.permissions...)
}

// acpFixture is a saige ACP server connected to the SDK's client over
// in-memory pipes.
type acpFixture struct {
	t      *testing.T
	conn   *acp.ClientSideConnection
	client *acpTestClient
	srv    *acpServer
	binds  []acpBindRequest
	mu     sync.Mutex
}

// newACPFixture serves agents built by newAgent; each bind records its
// request.
func newACPFixture(t *testing.T, opts acpOptions, newAgent func(acpBindRequest) (*agentsdk.Agent, *types.ToolRegistry)) *acpFixture {
	t.Helper()
	f := &acpFixture{t: t, client: &acpTestClient{answer: "allow_once"}}
	if opts.agent == "" {
		opts.agent = "helper"
	}
	if opts.agents == nil {
		opts.agents = func() []string { return []string{"helper", "reviewer"} }
	}
	if opts.models == nil {
		opts.models = []string{"fast", "smart"}
	}
	opts.bind = func(_ context.Context, req acpBindRequest) (agenthost.Agent, error) {
		f.mu.Lock()
		f.binds = append(f.binds, req)
		f.mu.Unlock()
		a, reg := newAgent(req)
		return agenthost.Agent{Agent: a, Tools: reg, MaxGrant: types.GrantTool}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.srv = newACPServer(ctx, opts)
	agentIn, clientOut := io.Pipe()
	clientIn, agentOut := io.Pipe()
	asc := acp.NewAgentSideConnection(f.srv, agentOut, agentIn)
	f.srv.client = asc
	f.conn = acp.NewClientSideConnection(f.client, clientOut, clientIn)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	asc.SetLogger(quiet)
	f.conn.SetLogger(quiet)
	t.Cleanup(func() {
		f.srv.close()
		cancel()
		_ = clientOut.Close()
		_ = agentOut.Close()
	})
	return f
}

// scriptedACPAgent is an agent that replays responses with tools.
func scriptedACPAgent(p *agenttest.ScriptedProvider, tools ...types.Tool) func(acpBindRequest) (*agentsdk.Agent, *types.ToolRegistry) {
	return func(req acpBindRequest) (*agentsdk.Agent, *types.ToolRegistry) {
		opts := []agentsdk.AgentOption{agentsdk.WithApprovalPolicy(agentsdk.ApprovalPolicy{})}
		if req.tree != nil {
			opts = append(opts, agentsdk.WithTree(req.tree))
		}
		cfg := agentsdk.AgentConfig{Name: req.agent, Provider: p, MaxIter: 6}
		if len(tools) > 0 {
			cfg.Tools = types.NewToolRegistry(tools...)
		}
		return agentsdk.NewAgent(cfg, opts...), cfg.Tools
	}
}

func (f *acpFixture) newSession() acp.SessionId {
	f.t.Helper()
	ctx := context.Background()
	if _, err := f.conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}); err != nil {
		f.t.Fatal(err)
	}
	res, err := f.conn.NewSession(ctx, acp.NewSessionRequest{Cwd: f.t.TempDir(), McpServers: []acp.McpServer{}})
	if err != nil {
		f.t.Fatal(err)
	}
	return res.SessionId
}

func (f *acpFixture) prompt(sid acp.SessionId, blocks ...acp.ContentBlock) acp.PromptResponse {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := f.conn.Prompt(ctx, acp.PromptRequest{SessionId: sid, Prompt: blocks})
	if err != nil {
		f.t.Fatal(err)
	}
	return res
}

func agentText(updates []acp.SessionUpdate) string {
	var sb strings.Builder
	for _, u := range updates {
		if u.AgentMessageChunk != nil && u.AgentMessageChunk.Content.Text != nil {
			sb.WriteString(u.AgentMessageChunk.Content.Text.Text)
		}
	}
	return sb.String()
}

func TestACPInitialize(t *testing.T) {
	f := newACPFixture(t, acpOptions{sessionsDir: t.TempDir()}, scriptedACPAgent(&agenttest.ScriptedProvider{}))
	res, err := f.conn.Initialize(context.Background(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	if err != nil {
		t.Fatal(err)
	}
	c := res.AgentCapabilities
	if res.ProtocolVersion != 1 || !c.PromptCapabilities.Image || !c.PromptCapabilities.Audio || !c.PromptCapabilities.EmbeddedContext ||
		!c.LoadSession || c.SessionCapabilities.List == nil || !c.McpCapabilities.Http {
		t.Fatalf("initialize = %+v", res)
	}
	if len(res.AuthMethods) == 0 || res.AuthMethods[0].Agent == nil || res.AuthMethods[0].Agent.Id != acpAuthEnv {
		t.Fatalf("auth methods = %+v", res.AuthMethods)
	}
	if res.AgentInfo == nil || res.AgentInfo.Name != "saige" {
		t.Fatalf("agent info = %+v", res.AgentInfo)
	}
}

func TestACPNewSessionConfigOptions(t *testing.T) {
	f := newACPFixture(t, acpOptions{}, scriptedACPAgent(&agenttest.ScriptedProvider{}))
	if _, err := f.conn.Initialize(context.Background(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.conn.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "relative", McpServers: []acp.McpServer{}}); err == nil {
		t.Fatal("a relative cwd was accepted")
	}
	cwd := t.TempDir()
	res, err := f.conn.NewSession(context.Background(), acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{
		{Stdio: &acp.McpServerStdio{Name: "files", Command: "files-mcp", Args: []string{"--ro"}, Env: []acp.EnvVariable{}}},
		{Http: &acp.McpServerHttpInline{Name: "web", Type: "http", Url: "https://example.test/mcp", Headers: []acp.HttpHeader{{Name: "X-Key", Value: "k"}}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res.SessionId == "" || len(res.ConfigOptions) != 2 || res.Modes != nil {
		t.Fatalf("new session = %+v", res)
	}
	agentOpt, modelOpt := res.ConfigOptions[0].Select, res.ConfigOptions[1].Select
	if agentOpt.Id != acpConfigAgent || agentOpt.CurrentValue != "helper" || len(*agentOpt.Options.Ungrouped) != 2 {
		t.Fatalf("agent option = %+v", agentOpt)
	}
	if modelOpt.Id != acpConfigModel || modelOpt.CurrentValue != acpModelDefault || len(*modelOpt.Options.Ungrouped) != 3 {
		t.Fatalf("model option = %+v", modelOpt)
	}
	// The client's servers reach the binding, which narrows them.
	req := f.binds[0]
	if req.cwd != cwd || len(req.servers) != 2 || req.servers[0].Command != "files-mcp" || req.servers[1].Headers["X-Key"] != "k" {
		t.Fatalf("bind request = %+v", req)
	}
}

func TestACPPromptTextAndImage(t *testing.T) {
	p := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("a red square")}}
	f := newACPFixture(t, acpOptions{}, scriptedACPAgent(p))
	sid := f.newSession()
	png := []byte("\x89PNG\r\n\x1a\nfake")
	res := f.prompt(sid,
		acp.TextBlock("what is this?"),
		acp.ImageBlock(base64.StdEncoding.EncodeToString(png), "image/png"),
		acp.ResourceBlock(acp.EmbeddedResourceResource{TextResourceContents: &acp.TextResourceContents{Uri: "file:///notes.md", Text: "remember"}}),
		acp.ResourceLinkBlock("main.go", "file:///src/main.go"),
	)
	if res.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stop = %s", res.StopReason)
	}
	updates, _ := f.client.snapshot()
	if got := agentText(updates); got != "a red square" {
		t.Fatalf("streamed text = %q", got)
	}
	msgs := p.Requests()[0].Messages
	user, ok := msgs[len(msgs)-1].(types.UserMessage)
	if !ok || len(user.Parts) != 4 {
		t.Fatalf("user message = %#v", msgs[len(msgs)-1])
	}
	if tp, ok := user.Parts[0].(types.TextPart); !ok || tp.Text != "what is this?" {
		t.Fatalf("part 0 = %#v", user.Parts[0])
	}
	if ip, ok := user.Parts[1].(types.ImagePart); !ok || string(ip.Source.Inline) != string(png) || ip.Source.MediaType != "image/png" {
		t.Fatalf("part 1 = %#v", user.Parts[1])
	}
	if tp, ok := user.Parts[2].(types.TextPart); !ok || !strings.Contains(tp.Text, "file:///notes.md") || !strings.Contains(tp.Text, "remember") {
		t.Fatalf("part 2 = %#v", user.Parts[2])
	}
	if tp, ok := user.Parts[3].(types.TextPart); !ok || !strings.Contains(tp.Text, "file:///src/main.go") {
		t.Fatalf("part 3 = %#v", user.Parts[3])
	}
}

func TestACPPermissionRoundTrip(t *testing.T) {
	tests := []struct {
		name     string
		answer   string
		wantRuns int32
		wantAsks int
	}{
		{"allow once asks each time", "allow_once", 2, 2},
		{"always allow grants the tool", "allow_always", 2, 1},
		{"reject", "reject_once", 0, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			p := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
				agenttest.ToolCallResponse("c1", "danger", map[string]any{"path": "a.txt"}),
				agenttest.ToolCallResponse("c2", "danger", map[string]any{"path": "b.txt"}),
				agenttest.TextResponse("finished"),
			}}
			f := newACPFixture(t, acpOptions{}, scriptedACPAgent(p, countedDanger(&calls)))
			f.client.answer = tt.answer
			sid := f.newSession()
			if res := f.prompt(sid, acp.TextBlock("go")); res.StopReason != acp.StopReasonEndTurn {
				t.Fatalf("stop = %s", res.StopReason)
			}
			if calls.Load() != tt.wantRuns {
				t.Fatalf("runs = %d, want %d", calls.Load(), tt.wantRuns)
			}
			updates, perms := f.client.snapshot()
			if len(perms) != tt.wantAsks {
				t.Fatalf("asked %d times, want %d", len(perms), tt.wantAsks)
			}
			first := perms[0]
			if first.ToolCall.ToolCallId != "c1" || first.ToolCall.Title == nil || *first.ToolCall.Title != "approve danger" {
				t.Fatalf("permission request = %+v", first)
			}
			kinds := map[acp.PermissionOptionKind]bool{}
			for _, o := range first.Options {
				kinds[o.Kind] = true
			}
			if !kinds[acp.PermissionOptionKindAllowOnce] || !kinds[acp.PermissionOptionKindAllowAlways] || !kinds[acp.PermissionOptionKindRejectOnce] {
				t.Fatalf("options = %+v", first.Options)
			}
			// The tool call streams as a call with updates and an end status.
			var started, ended bool
			for _, u := range updates {
				if u.ToolCall != nil && u.ToolCall.ToolCallId == "c1" {
					started = u.ToolCall.Title != ""
				}
				if u.ToolCallUpdate != nil && u.ToolCallUpdate.ToolCallId == "c1" && u.ToolCallUpdate.Status != nil &&
					(*u.ToolCallUpdate.Status == acp.ToolCallStatusCompleted || *u.ToolCallUpdate.Status == acp.ToolCallStatusFailed) {
					ended = true
				}
			}
			if !started || !ended {
				t.Fatalf("tool call updates: started=%v ended=%v", started, ended)
			}
			if got := agentText(updates); got != "finished" {
				t.Fatalf("text = %q", got)
			}
		})
	}
}

func TestACPAlwaysAllowRespectsTheGrantCap(t *testing.T) {
	var calls atomic.Int32
	p := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("c1", "danger", map[string]any{}),
		agenttest.TextResponse("finished"),
	}}
	f := newACPFixture(t, acpOptions{}, scriptedACPAgent(p, countedDanger(&calls)))
	bind := f.srv.opts.bind
	f.srv.opts.bind = func(ctx context.Context, req acpBindRequest) (agenthost.Agent, error) {
		a, err := bind(ctx, req)
		a.MaxGrant = types.GrantOnce
		return a, err
	}
	sid := f.newSession()
	f.prompt(sid, acp.TextBlock("go"))
	_, perms := f.client.snapshot()
	for _, o := range perms[0].Options {
		if o.Kind == acp.PermissionOptionKindAllowAlways {
			t.Fatalf("always allow offered under a once cap: %+v", perms[0].Options)
		}
	}
}

func TestACPCancel(t *testing.T) {
	started := make(chan struct{})
	wait := &types.ToolFunc{
		Def: types.ToolDef{Name: "wait", Parameters: types.ParameterSchema{Type: "object"}, Capability: types.ToolCapabilityRead},
		Fn: func(ctx context.Context, _ map[string]any) (string, error) {
			close(started)
			<-ctx.Done()
			return "", ctx.Err()
		},
	}
	p := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("c1", "wait", map[string]any{}),
		agenttest.TextResponse("never"),
	}}
	f := newACPFixture(t, acpOptions{}, scriptedACPAgent(p, wait))
	sid := f.newSession()
	go func() {
		<-started
		_ = f.conn.Cancel(context.Background(), acp.CancelNotification{SessionId: sid})
	}()
	if res := f.prompt(sid, acp.TextBlock("wait")); res.StopReason != acp.StopReasonCancelled {
		t.Fatalf("stop = %s", res.StopReason)
	}
	// The session takes another turn after a cancel.
	if f.srv.sessions.Get(string(sid)).Running() {
		t.Fatal("the cancelled turn is still running")
	}
}

func TestACPCancelWhilePermissionPending(t *testing.T) {
	var calls atomic.Int32
	p := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("c1", "danger", map[string]any{}),
		agenttest.TextResponse("never"),
	}}
	f := newACPFixture(t, acpOptions{}, scriptedACPAgent(p, countedDanger(&calls)))
	f.client.answer = ""
	sid := f.newSession()
	go func() {
		for {
			if _, perms := f.client.snapshot(); len(perms) > 0 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		_ = f.conn.Cancel(context.Background(), acp.CancelNotification{SessionId: sid})
	}()
	if res := f.prompt(sid, acp.TextBlock("go")); res.StopReason != acp.StopReasonCancelled {
		t.Fatalf("stop = %s", res.StopReason)
	}
	if calls.Load() != 0 {
		t.Fatal("a cancelled approval ran the tool")
	}
}

func TestACPSetConfigOption(t *testing.T) {
	p := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("one"), agenttest.TextResponse("two"), agenttest.TextResponse("three")}}
	f := newACPFixture(t, acpOptions{}, scriptedACPAgent(p))
	sid := f.newSession()
	f.prompt(sid, acp.TextBlock("first"))
	set := func(id, value string) (acp.SetSessionConfigOptionResponse, error) {
		return f.conn.SetSessionConfigOption(context.Background(), acp.SetSessionConfigOptionRequest{ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: sid, ConfigId: acp.SessionConfigId(id), Value: acp.SessionConfigValueId(value),
		}})
	}
	if _, err := set(acpConfigModel, "unknown"); err == nil {
		t.Fatal("an unknown model was accepted")
	}
	res, err := set(acpConfigModel, "smart")
	if err != nil || res.ConfigOptions[1].Select.CurrentValue != "smart" {
		t.Fatalf("set model: %+v %v", res, err)
	}
	// A new model keeps the conversation.
	f.prompt(sid, acp.TextBlock("second"))
	if n := len(p.Requests()[1].Messages); n < 4 {
		t.Fatalf("the conversation was not kept: %d messages", n)
	}
	last := f.binds[len(f.binds)-1]
	if last.model != "smart" || last.tree == nil {
		t.Fatalf("bind = %+v", last)
	}
	// A new agent starts a fresh conversation with the default model.
	if res, err = set(acpConfigAgent, "reviewer"); err != nil || res.ConfigOptions[0].Select.CurrentValue != "reviewer" || res.ConfigOptions[1].Select.CurrentValue != acpModelDefault {
		t.Fatalf("set agent: %+v %v", res, err)
	}
	last = f.binds[len(f.binds)-1]
	if last.agent != "reviewer" || last.tree != nil {
		t.Fatalf("bind = %+v", last)
	}
}

func TestACPLoadSession(t *testing.T) {
	dir := t.TempDir()
	p := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("hello there"), agenttest.TextResponse("again")}}
	f := newACPFixture(t, acpOptions{sessionsDir: dir}, scriptedACPAgent(p))
	sid := f.newSession()
	f.prompt(sid, acp.TextBlock("hi"))
	if _, err := os.Stat(filepath.Join(dir, string(sid)+".json")); err != nil {
		t.Fatalf("session not saved: %v", err)
	}

	// A new connection, as after an editor restart, loads it.
	g := newACPFixture(t, acpOptions{sessionsDir: dir}, scriptedACPAgent(p))
	if _, err := g.conn.Initialize(context.Background(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}); err != nil {
		t.Fatal(err)
	}
	list, err := g.conn.ListSessions(context.Background(), acp.ListSessionsRequest{})
	if err != nil || len(list.Sessions) != 1 || list.Sessions[0].SessionId != sid || list.Sessions[0].Title == nil || *list.Sessions[0].Title != "hi" {
		t.Fatalf("list = %+v %v", list, err)
	}
	if _, err := g.conn.LoadSession(context.Background(), acp.LoadSessionRequest{SessionId: sid, Cwd: t.TempDir(), McpServers: []acp.McpServer{}}); err != nil {
		t.Fatal(err)
	}
	updates, _ := g.client.snapshot()
	var user string
	for _, u := range updates {
		if u.UserMessageChunk != nil && u.UserMessageChunk.Content.Text != nil {
			user += u.UserMessageChunk.Content.Text.Text
		}
	}
	if user != "hi" || agentText(updates) != "hello there" {
		t.Fatalf("replay: user=%q agent=%q", user, agentText(updates))
	}
	g.prompt(sid, acp.TextBlock("more"))
	if n := len(p.Requests()[1].Messages); n < 4 {
		t.Fatalf("the loaded conversation was not continued: %d messages", n)
	}
	if _, err := g.conn.LoadSession(context.Background(), acp.LoadSessionRequest{SessionId: "sess_../../etc", Cwd: t.TempDir(), McpServers: []acp.McpServer{}}); err == nil {
		t.Fatal("a malformed session id was loaded")
	}
}

func TestACPUserMessageRejectsEmptyAndBadData(t *testing.T) {
	if _, err := acpUserMessage(nil); err == nil {
		t.Fatal("empty prompt accepted")
	}
	if _, err := acpUserMessage([]acp.ContentBlock{acp.ImageBlock("%%%", "image/png")}); err == nil {
		t.Fatal("bad base64 accepted")
	}
	msg, err := acpUserMessage([]acp.ContentBlock{
		acp.AudioBlock(base64.StdEncoding.EncodeToString([]byte("RIFF")), "audio/wav"),
		acp.ResourceBlock(acp.EmbeddedResourceResource{BlobResourceContents: &acp.BlobResourceContents{
			Uri: "file:///r/report.pdf", Blob: base64.StdEncoding.EncodeToString([]byte("%PDF")), MimeType: acp.Ptr("application/pdf"),
		}}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := msg.Parts[0].(types.AudioPart); !ok {
		t.Fatalf("part 0 = %#v", msg.Parts[0])
	}
	if d, ok := msg.Parts[1].(types.DocumentPart); !ok || d.Source.Filename != "report.pdf" {
		t.Fatalf("part 1 = %#v", msg.Parts[1])
	}
}
