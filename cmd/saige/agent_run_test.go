package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/tui"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

func TestAgentRunRestoresSession(t *testing.T) {
	var stdout, stderr bytes.Buffer
	o := agentRunOptions{sessionID: "research", sessionsDir: t.TempDir(), identity: "sha256:one", trace: true, diagnostics: &stderr}
	out := tui.ResolveOutputWriters(false, tui.TemplateMinimal, &stdout, &stderr)
	first := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("Remembered.")}}
	a := must.Get(agentsdk.New(agentsdk.Config{Provider: first}))
	if err := runAgentTurn(context.Background(), a, "My code is violet.", out, false, o); err != nil {
		t.Fatal(err)
	}
	saved := must.Get(agentsdk.LoadSessionFromFile(filepath.Join(o.sessionsDir, "research.json")))
	second := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("violet")}}
	b := must.Get(agentsdk.New(agentsdk.Config{Provider: second}))
	if err := runAgentTurn(context.Background(), b, "What is my code?", out, false, o); err != nil {
		t.Fatal(err)
	}
	requests := second.Requests()
	data, err := json.Marshal(requests[0].Messages)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("My code is violet.")) || !bytes.Contains(data, []byte("Remembered.")) {
		t.Fatalf("history missing: %s", data)
	}
	restored := must.Get(agentsdk.LoadSessionFromFile(filepath.Join(o.sessionsDir, "research.json")))
	if restored.ID != saved.ID || !restored.CreatedAt.Equal(saved.CreatedAt) {
		t.Fatal("session identity changed")
	}
	if !strings.Contains(stderr.String(), `"restored":true`) || !strings.Contains(stderr.String(), `"messages":3`) {
		t.Fatalf("missing restore trace: %s", &stderr)
	}
	if strings.Contains(stdout.String(), "session_loaded") {
		t.Fatal("trace polluted stdout")
	}
	info := must.Get(os.Stat(filepath.Join(o.sessionsDir, "research.json")))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("session mode %v", info.Mode())
	}
	o.identity = "sha256:changed"
	if err := runAgentTurn(context.Background(), b, "must not run", out, false, o); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("mismatched identity: %v", err)
	}
	if second.CallCount() != 1 {
		t.Fatal("mismatched session called provider")
	}
}

func TestAgentRunSessionFailures(t *testing.T) {
	for _, id := range []string{"../escape", "/absolute", "a/b", ".", ""} {
		if _, unlock, err := openRunSession(agentRunOptions{sessionID: id, sessionsDir: t.TempDir()}); err == nil {
			unlock()
			t.Fatalf("accepted %q", id)
		}
	}
	o := agentRunOptions{sessionID: "locked", sessionsDir: t.TempDir()}
	_, unlock, err := openRunSession(o)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, release, err := openRunSession(o); err == nil {
		release()
		t.Fatal("concurrent session opened")
	}
}

func TestAgentRunDoesNotOverwriteCorruptSession(t *testing.T) {
	o := agentRunOptions{sessionID: "broken", sessionsDir: t.TempDir(), identity: "test"}
	path := filepath.Join(o.sessionsDir, "broken.json")
	if err := os.WriteFile(path, []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &agenttest.ScriptedProvider{}
	a := must.Get(agentsdk.New(agentsdk.Config{Provider: p}))
	var output bytes.Buffer
	err := runAgentTurn(context.Background(), a, "hi", tui.ResolveOutputWriters(false, tui.TemplateMinimal, &output, &output), false, o)
	if err == nil || p.CallCount() != 0 {
		t.Fatalf("corrupt session: %v, calls %d", err, p.CallCount())
	}
	if got := string(must.Get(os.ReadFile(path))); got != "broken" {
		t.Fatalf("overwrote corrupt session: %q", got)
	}
}

func TestAgentRunTraceRoutesAndTools(t *testing.T) {
	for _, jsonMode := range []bool{false, true} {
		t.Run(fmt.Sprint(jsonMode), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			response := append([]types.Delta{types.RouteDelta{Provider: "test", Model: "local"}}, agenttest.ToolCallResponse("lookup", "search", map[string]any{"query": "private-query"})...)
			p := &agenttest.ScriptedProvider{Responses: [][]types.Delta{response, agenttest.TextResponse("answer")}}
			tool := &types.ToolFunc{Def: types.ToolDef{Name: "search"}, Fn: func(context.Context, map[string]any) (string, error) { return "private-result", nil }}
			a := must.Get(agentsdk.New(agentsdk.Config{Provider: p, Tools: types.NewToolRegistry(tool)}))
			out := tui.ResolveOutputWriters(jsonMode, tui.TemplateMinimal, &stdout, &stderr)
			if err := runAgentTurn(context.Background(), a, "question", out, false, agentRunOptions{trace: true, diagnostics: &stderr}); err != nil {
				t.Fatal(err)
			}
			for _, event := range []string{"agent_loaded", "provider", "tool_started", "tool_finished"} {
				if !strings.Contains(stderr.String(), `"event":"`+event+`"`) {
					t.Errorf("missing %s: %s", event, &stderr)
				}
				if strings.Contains(stdout.String(), `"event":"`+event+`"`) {
					t.Errorf("trace on stdout: %s", &stdout)
				}
			}
			if strings.Contains(stderr.String(), "private-") {
				t.Fatalf("trace disclosed tool payload: %s", &stderr)
			}
			decoder := json.NewDecoder(&stderr)
			for decoder.More() {
				var event map[string]any
				if err := decoder.Decode(&event); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
