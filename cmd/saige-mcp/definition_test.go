package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	agenttypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

func writeDefinition(t *testing.T, dir, name, extra string) {
	t.Helper()
	body := "---\napiVersion: saige/v1\nname: " + name + "\nversion: 1.0.0\ndescription: The " + name + " agent.\n" +
		"model: anthropic/claude-haiku-5-5\n" + extra + "---\nYou are " + name + ".\n"
	if err := os.WriteFile(filepath.Join(dir, name+".agent.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDefinitionTool(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	dir := t.TempDir()
	writeDefinition(t, dir, "searcher", "tools:\n  registry: [kg_search]\n")
	writeDefinition(t, dir, "editor", "tools:\n  harness: [write]\n")
	packs := agenttypes.NewToolRegistry(&agenttest.MockTool{Def: agenttypes.ToolDef{Name: "kg_search", Capability: agenttypes.ToolCapabilityRead}})
	flags := func(ref string, set ...string) definitionFlags {
		f := definitionFlags{agentFlags: agentFlags{ref: ref, name: defaultAgentTool}, dir: dir, root: t.TempDir(), set: map[string]bool{}}
		for _, s := range set {
			f.set[s] = true
		}
		return f
	}

	at, ok, err := newDefinitionTool(context.Background(), flags("searcher"), packs)
	if err != nil || !ok {
		t.Fatalf("got %v %v", ok, err)
	}
	if at.name != "searcher" || at.description != "The searcher agent." || at.gated || at.newSession == nil {
		t.Fatalf("tool %+v", at)
	}
	ag, err := at.newSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info := ag.Agent.Info(); info.Name != "searcher" || len(info.Tools) != 1 || info.Tools[0] != "kg_search" {
		t.Fatalf("agent %+v", info)
	}
	ag.Release()

	// Writes carry markers, so the tool is published as gated.
	at, _, err = newDefinitionTool(context.Background(), flags("editor@^1"), packs)
	if err != nil || !at.gated {
		t.Fatalf("editor: %+v %v", at, err)
	}

	// Explicit flags win; an unknown name falls back to a preset.
	f := flags("searcher", "agent-tool", "agent-description")
	f.name, f.description = "lookup", "Look things up."
	if at, _, _ = newDefinitionTool(context.Background(), f, packs); at.name != "lookup" || at.description != "Look things up." {
		t.Fatalf("flags ignored: %+v", at)
	}
	if _, ok, err := newDefinitionTool(context.Background(), flags("anthropic"), packs); ok || err != nil {
		t.Fatalf("a preset name was taken for a definition: %v %v", ok, err)
	}
	if _, _, err := newDefinitionTool(context.Background(), flags("searcher", "agent-system"), packs); err == nil {
		t.Fatal("--agent-system was combined with a definition")
	}
	clash := agenttypes.NewToolRegistry(&agenttest.MockTool{Def: agenttypes.ToolDef{Name: "searcher"}}, &agenttest.MockTool{Def: agenttypes.ToolDef{Name: "kg_search"}})
	if _, _, err := newDefinitionTool(context.Background(), flags("searcher"), clash); err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("got %v", err)
	}
	writeDefinition(t, dir, "broken", "tools:\n  registry: [nowhere]\n")
	if _, _, err := newDefinitionTool(context.Background(), flags("broken"), packs); err == nil || !strings.Contains(err.Error(), "nowhere") {
		t.Fatalf("got %v", err)
	}
}

// TestAgentToolBindsPerCall checks that a definition-backed agent tool binds
// for each call and releases the binding afterwards.
func TestAgentToolBindsPerCall(t *testing.T) {
	var binds, releases atomic.Int32
	p := &agenttest.ScriptedProvider{Responses: [][]agenttypes.Delta{agenttest.TextResponse("one"), agenttest.TextResponse("two")}}
	at := agentTool{name: defaultAgentTool, description: "test", newBound: func(context.Context) (*agentsdk.Agent, func(), error) {
		binds.Add(1)
		return must.Get(agentsdk.New(agentsdk.Config{Name: "t", Provider: p})), func() { releases.Add(1) }, nil
	}}
	cs := agentSession(t, bridge{approval: approvalElicit}, at, nil)
	for _, want := range []string{"one", "two"} {
		res := callAgent(t, cs, "go")
		if text := res.Content[0].(*mcp.TextContent).Text; text != want {
			t.Fatalf("got %q, want %q", text, want)
		}
	}
	if binds.Load() != 2 || releases.Load() != 2 {
		t.Fatalf("binds %d releases %d", binds.Load(), releases.Load())
	}
}
