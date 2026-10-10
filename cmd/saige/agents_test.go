package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/cmd/internal/agenthost"
	"github.com/urmzd/saige/internal/must"
)

// agentsSandbox isolates discovery from the developer's own definitions
// and gives model adapters a placeholder key, so binding never calls out.
func agentsSandbox(t *testing.T) (home, project string) {
	t.Helper()
	home, project = catalogSandbox(t)
	t.Setenv(envTrustProjectAgents, "")
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("OPENAI_API_KEY", "")
	*agentFlags.dirs, *agentFlags.mcpConfig = nil, ""
	return home, project
}

func definitionFile(name, version, extra, prompt string) string {
	return "---\napiVersion: saige/v1\nname: " + name + "\nversion: " + version + "\n" + extra + "---\n" + prompt + "\n"
}

// exampleAgents is the shipped examples directory, resolved before any
// test changes the working directory.
var exampleAgents, _ = filepath.Abs(filepath.Join("..", "..", "examples", "agents"))

func examplesDir(*testing.T) string { return exampleAgents }

func TestDiscoverAgentLayers(t *testing.T) {
	home, project := agentsSandbox(t)
	user := filepath.Join(home, ".config", "saige", "agents")
	proj := filepath.Join(project, ".saige", "agents")
	writeFile(t, filepath.Join(user, "a.agent.md"), definitionFile("a", "1.0.0", "", "a"))
	writeFile(t, filepath.Join(proj, "b.md"), definitionFile("b", "1.0.0", "", "b"))
	extra := t.TempDir()

	layers := discoverAgentLayers([]string{extra}, os.Getenv)
	var kinds []string
	for _, l := range layers {
		kinds = append(kinds, l.Kind)
	}
	if strings.Join(kinds, ",") != "user,project,flag" || layers[1].Trusted || !layers[0].Trusted || !layers[2].Trusted {
		t.Fatalf("layers %+v", layers)
	}
	// Naming the project directory trusts it and loads it once.
	layers = discoverAgentLayers([]string{proj}, os.Getenv)
	if len(layers) != 2 || layers[1].Kind != layerFlag || !layers[1].Trusted {
		t.Fatalf("layers %+v", layers)
	}
	t.Setenv(envTrustProjectAgents, "1")
	if layers = discoverAgentLayers(nil, os.Getenv); !layers[1].Trusted {
		t.Fatalf("trusted by environment: %+v", layers)
	}
}

func TestAgentCommands(t *testing.T) {
	agentsSandbox(t)
	dir := examplesDir(t)

	code, out := runCLI(t, "agent", "list", "--agents-dir", dir)
	if code != 0 || !strings.Contains(out, "assistant") || !strings.Contains(out, "repo-steward") || !strings.Contains(out, "1.2.0") {
		t.Fatalf("list: %d %s", code, out)
	}
	code, out = runCLI(t, "agent", "list", "--agents-dir", dir, "--format", "json")
	var listedOut struct {
		Agents []listedAgent `json:"agents"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &listedOut) != nil || len(listedOut.Agents) != 2 || !strings.HasPrefix(listedOut.Agents[0].Digest, "sha256:") {
		t.Fatalf("list json: %d %s", code, out)
	}

	code, out = runCLI(t, "agent", "show", "repo-steward@^1", "--agents-dir", dir)
	if code != 0 || !strings.Contains(out, "resolved: sha256:") || !strings.Contains(out, "subagent: assistant@^1.0 -> assistant@1.0.0 (delegate)") ||
		!strings.Contains(out, "Bash(git status:*)") {
		t.Fatalf("show: %d %s", code, out)
	}
	code, out = runCLI(t, "agent", "show", "repo-steward", "--agents-dir", dir, "--format", "json")
	var shown shownAgent
	if code != 0 || json.Unmarshal([]byte(out), &shown) != nil || shown.ResolvedDigest == shown.Digest || len(shown.Subagents) != 1 {
		t.Fatalf("show json: %d %s", code, out)
	}
	if code, out = runCLI(t, "agent", "show", "ghost", "--agents-dir", dir); code != exitInvalid || !strings.Contains(out, "ghost") {
		t.Fatalf("show missing: %d %s", code, out)
	}

	if code, out = runCLI(t, "agent", "schema"); code != 0 || !strings.Contains(out, `"$schema"`) || !strings.Contains(out, "saige/v1") {
		t.Fatalf("schema: %d %s", code, out)
	}

	if code, out = runCLI(t, "agent", "validate", dir); code != 0 || !strings.Contains(out, "2 agent definitions valid") {
		t.Fatalf("validate: %d %s", code, out)
	}
	bad := writeFile(t, filepath.Join(t.TempDir(), "bad.agent.md"), definitionFile("bad", "1.0.0", "subagents: [ghost]\n", "x"))
	if code, out = runCLI(t, "agent", "validate", bad); code != exitInvalid || !strings.Contains(out, `no definition named "ghost"`) {
		t.Fatalf("validate bad: %d %s", code, out)
	}
	model := writeFile(t, filepath.Join(t.TempDir(), "model.agent.md"), definitionFile("m", "1.0.0", "model: no-such-preset\n", "x"))
	if code, out = runCLI(t, "agent", "validate", model); code != exitInvalid || !strings.Contains(out, "no-such-preset") {
		t.Fatalf("validate model: %d %s", code, out)
	}
}

func TestAgentUntrustedProject(t *testing.T) {
	_, project := agentsSandbox(t)
	writeFile(t, filepath.Join(project, ".saige", "agents", "runner.md"), definitionFile("runner", "1.0.0", "tools:\n  harness: [exec]\n", "x"))
	code, out := runCLI(t, "agent", "list")
	if code != exitInvalid || !strings.Contains(out, "untrusted") || !strings.Contains(out, "tools.harness[0]") {
		t.Fatalf("untrusted: %d %s", code, out)
	}
	t.Setenv(envTrustProjectAgents, "1")
	if code, out = runCLI(t, "agent", "list"); code != 0 || !strings.Contains(out, "runner") {
		t.Fatalf("trusted: %d %s", code, out)
	}
}

func TestAskAgentFlagErrors(t *testing.T) {
	agentsSandbox(t)
	dir := examplesDir(t)
	cases := []struct {
		args []string
		code int
		want string
	}{
		{[]string{"ask", "--agents-dir", dir, "--agent", "ghost", "hi"}, exitInvalid, `no definition named "ghost"`},
		{[]string{"ask", "--agents-dir", dir, "--agent", "assistant@^2", "hi"}, exitInvalid, "available: 1.0.0"},
		{[]string{"ask", "--agents-dir", dir, "--agent", "assistant", "--tools", "read", "hi"}, 1, "--tools and --agent"},
		{[]string{"ask", "--agents-dir", dir, "--agent", "assistant", "--system", "x", "hi"}, 1, "--system and --agent"},
		{[]string{"chat", "--agents-dir", dir, "--agent", "assistant", "--tools", "read"}, 1, "--tools and --agent"},
	}
	for _, tc := range cases {
		code, out := runCLI(t, tc.args...)
		if code != tc.code || !strings.Contains(out, tc.want) {
			t.Errorf("%v: %d %s", tc.args, code, out)
		}
	}
}

func TestBindCLIAgent(t *testing.T) {
	agentsSandbox(t)
	resetCatalogCache()
	root := newRootCmd(context.Background())
	cmd, _, err := root.Find([]string{"ask"})
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	if err := cmd.ParseFlags([]string{"--agents-dir", examplesDir(t), "--workspace", workspace, "--exec-network", "allow"}); err != nil {
		t.Fatal(err)
	}
	hf := harnessFlags{workspace: workspace, sandbox: "subprocess", network: "allow"}
	run, err := bindCLIAgent(context.Background(), cmd, persistentFlagVars, "repo-steward", hf.options(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer run.cleanup()
	cfg := run.bound.Config
	if cfg.Name != "repo-steward" || !strings.Contains(cfg.SystemPrompt, "git repository") || len(cfg.SubAgents) != 1 ||
		cfg.SubAgents[0].Name != "assistant" || cfg.MaxIter != 10 || cfg.Budget == nil {
		t.Fatalf("config %+v", cfg)
	}
	var names []string
	for _, d := range cfg.Tools.Definitions() {
		names = append(names, d.Name)
	}
	if !slices.Contains(names, "execute_code") || !slices.Contains(names, "read_file") {
		t.Fatalf("tools %v", names)
	}
	code, _ := cfg.Tools.Get("execute_code")
	allowed := cfg.ToolGate.Check(context.Background(), code.Definition(), map[string]any{"language": "shell", "code": "git status"})
	pushed := cfg.ToolGate.Check(context.Background(), code.Definition(), map[string]any{"language": "shell", "code": "git push origin main"})
	if allowed.Outcome != types.GateAllow || pushed.Outcome != types.GateDeny {
		t.Fatalf("gate: %v %v", allowed, pushed)
	}
	if run.bound.MaxGrant != types.GrantTool || run.bound.Pin().Version != "1.2.0" {
		t.Fatalf("bound %+v", run.bound.Pin())
	}
}

func TestServeSessionAgent(t *testing.T) {
	calls, released := &atomic.Int32{}, &atomic.Int32{}
	opts := serveOptions{approvalTimeout: 2 * time.Second}
	opts.newSessionAgent = func() (agenthost.Agent, error) {
		a := must.Get(agentsdk.New(agentsdk.Config{
			Name: "pinned",
			Provider: &agenttest.ScriptedProvider{Responses: [][]types.Delta{
				agenttest.ToolCallResponse("call_1", "danger", map[string]any{}),
				agenttest.TextResponse("finished"),
			}},
			Tools: types.NewToolRegistry(countedDanger(calls)),
		}, agentsdk.WithApprovalPolicy(agentsdk.ApprovalPolicy{})))
		return agenthost.Agent{
			Agent:   a,
			Release: func() { released.Add(1) },
			CheckGrant: func(g *types.GrantRequest) error {
				if g != nil && g.Scope == types.GrantSession {
					return errors.New("session grants are not allowed")
				}
				return nil
			},
			Info: map[string]string{"name": "pinned", "digest": "sha256:abc"},
		}, nil
	}
	f := newServeFixtureWith(t, opts, calls)
	created := f.post("/v1/sessions", map[string]any{}, http.StatusCreated)
	info, _ := created["agent"].(map[string]any)
	if info["digest"] != "sha256:abc" {
		t.Fatalf("created %v", created)
	}
	sid := created["session_id"].(string)
	tid := f.post("/v1/sessions/"+sid+"/turns", map[string]any{"message": "go"}, http.StatusAccepted)["turn_id"].(string)
	f.events(sid, tid, "", func(fr sseFrame) bool { return fr.kind == types.WireMarker })
	path := "/v1/sessions/" + sid + "/turns/" + tid + "/interrupts/call_1"
	if out := f.post(path, map[string]any{"approved": true, "grant": map[string]any{"scope": "session"}}, http.StatusBadRequest); !strings.Contains(out["error"].(string), "session grants") {
		t.Fatalf("got %v", out)
	}
	f.post(path, map[string]any{"approved": true, "grant": map[string]any{"scope": "tool"}}, http.StatusOK)
	waitDone(t, f, sid, tid)
	if calls.Load() != 1 {
		t.Fatalf("tool ran %d times", calls.Load())
	}
	f.do(http.MethodDelete, "/v1/sessions/"+sid, nil, http.StatusOK)
	if released.Load() != 1 {
		t.Fatalf("released %d times", released.Load())
	}
}
