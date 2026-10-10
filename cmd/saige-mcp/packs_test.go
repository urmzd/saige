package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	saigemcp "github.com/urmzd/saige/agent/mcp"
	agenttypes "github.com/urmzd/saige/agent/types"
)

func TestParsePacks(t *testing.T) {
	tests := []struct {
		in      string
		want    []string
		wantErr bool
	}{
		{"all", allPacks, false},
		{"research, KG", []string{packResearch, packKG}, false},
		{"eval,mcp", []string{packEval, packMCP}, false},
		{"none", nil, false},
		{"reserch", nil, true},
	}
	for _, tt := range tests {
		got, err := parsePacks(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("parsePacks(%q) err = %v", tt.in, err)
			continue
		}
		var names []string
		for name := range got {
			names = append(names, name)
		}
		slices.Sort(names)
		want := slices.Clone(tt.want)
		slices.Sort(want)
		if !slices.Equal(names, want) {
			t.Errorf("parsePacks(%q) = %v, want %v", tt.in, names, want)
		}
	}
}

func registryNames(t *testing.T, cfg packConfig) []string {
	t.Helper()
	reg, cleanup, err := buildRegistry(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	var names []string
	for _, d := range reg.Definitions() {
		names = append(names, d.Name)
	}
	slices.Sort(names)
	return names
}

func TestBuildRegistrySelectsPacks(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(cfgFile, []byte(`{"mcpServers": {"docs": {"type": "http", "url": "http://127.0.0.1:1/mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	tests := []struct {
		name string
		cfg  packConfig
		want []string
	}{
		{"eval", packConfig{packs: map[string]bool{packEval: true}}, []string{"eval_compare", "eval_run"}},
		{"mcp", packConfig{packs: map[string]bool{packMCP: true}, mcpConfig: cfgFile}, []string{"mcp_catalog", "mcp_probe"}},
		{"mcp without a config is skipped", packConfig{packs: map[string]bool{packMCP: true}}, nil},
		{"research without a database", packConfig{packs: map[string]bool{packResearch: true}, root: root}, []string{"file_search", "read_file"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := registryNames(t, tt.cfg); !slices.Equal(got, tt.want) {
				t.Fatalf("tools = %v, want %v", got, tt.want)
			}
		})
	}
}

func runTool(t *testing.T, tool agenttypes.Tool, args string) (string, error) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(args), &m); err != nil {
		t.Fatal(err)
	}
	if err := agenttypes.ValidateToolArgs(tool.Definition().Parameters, m); err != nil {
		t.Fatalf("arguments do not match the schema: %v", err)
	}
	return tool.Execute(context.Background(), m)
}

func toolNamed(t *testing.T, tools []agenttypes.Tool, name string) agenttypes.Tool {
	t.Helper()
	for _, tool := range tools {
		if tool.Definition().Name == name {
			return tool
		}
	}
	t.Fatalf("no tool %s", name)
	return nil
}

func TestEvalRun(t *testing.T) {
	out, err := runTool(t, toolNamed(t, evalTools(), "eval_run"), `{
		"name": "capitals",
		"cases": [
			{"id": "fr", "output": "Paris", "ground_truth": "Paris"},
			{"id": "de", "output": "Bonn", "ground_truth": "Berlin"}
		],
		"scorers": [{"kind": "exact_match"}, {"kind": "contains", "name": "mentions_paris", "params": {"substrings": ["Paris"]}}]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	var suite struct {
		Name      string             `json:"name"`
		Aggregate map[string]float64 `json:"aggregate"`
		Results   []json.RawMessage  `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &suite); err != nil {
		t.Fatal(err)
	}
	if suite.Name != "capitals" || len(suite.Results) != 2 || suite.Aggregate["exact_match"] != 0.5 || suite.Aggregate["mentions_paris"] != 0.5 {
		t.Fatalf("suite = %s", out)
	}
}

func TestEvalRunRejectsBadInput(t *testing.T) {
	run := toolNamed(t, evalTools(), "eval_run")
	for name, args := range map[string]string{
		"unknown scorer": `{"cases": [{"id": "a", "output": "x"}], "scorers": [{"kind": "vibes"}]}`,
		"no cases":       `{"cases": [], "scorers": [{"kind": "exact_match"}]}`,
		"duplicate id":   `{"cases": [{"id": "a", "output": "x"}, {"id": "a", "output": "y"}], "scorers": [{"kind": "exact_match"}]}`,
		"missing id":     `{"cases": [{"id": "", "output": "x"}], "scorers": [{"kind": "exact_match"}]}`,
		"unknown field":  `{"cases": [{"id": "a", "output": "x"}], "scorers": [{"kind": "exact_match"}], "judge": true}`,
	} {
		var m map[string]any
		_ = json.Unmarshal([]byte(args), &m)
		if _, err := run.Execute(context.Background(), m); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestEvalCompare(t *testing.T) {
	out, err := runTool(t, toolNamed(t, evalTools(), "eval_compare"), `{
		"base": [{"id": "fr", "output": "Lyon", "ground_truth": "Paris"}, {"id": "de", "output": "Berlin", "ground_truth": "Berlin"}],
		"candidate": [{"id": "fr", "output": "Paris", "ground_truth": "Paris"}, {"id": "de", "output": "Berlin", "ground_truth": "Berlin"}],
		"scorers": [{"kind": "exact_match"}]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"exact_match"`) || !strings.Contains(out, `"fr"`) {
		t.Fatalf("comparison = %s", out)
	}
}

func TestMCPToolsProbeAndListConfiguredServers(t *testing.T) {
	// A real MCP server over streamable HTTP, with one tool.
	upstream := mcp.NewServer(&mcp.Implementation{Name: "upstream", Version: "1"}, nil)
	bridge{approval: approvalHost}.register(upstream, echoTool())
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return upstream }, nil))
	t.Cleanup(ts.Close)

	specs, err := saigemcp.ParseConfig([]byte(`{"mcpServers": {"up": {"type": "http", "url": "`+ts.URL+`"}}}`),
		saigemcp.WithConfigHTTPClient(nil))
	if err != nil {
		t.Fatal(err)
	}
	tools := mcpTools(specs)

	out, err := runTool(t, toolNamed(t, tools, "mcp_probe"), `{"server": "up"}`)
	if err != nil {
		t.Fatal(err)
	}
	var probes []struct {
		Server    string   `json:"server"`
		OK        bool     `json:"ok"`
		ToolCount int      `json:"tool_count"`
		Tools     []string `json:"tools"`
	}
	if err := json.Unmarshal([]byte(out), &probes); err != nil {
		t.Fatal(err)
	}
	if len(probes) != 1 || !probes[0].OK || probes[0].ToolCount != 1 || probes[0].Tools[0] != "echo" {
		t.Fatalf("probe = %s", out)
	}

	out, err = runTool(t, toolNamed(t, tools, "mcp_catalog"), `{"server": "up"}`)
	if err != nil {
		t.Fatal(err)
	}
	var cat saigemcp.Catalog
	if err := json.Unmarshal([]byte(out), &cat); err != nil {
		t.Fatal(err)
	}
	if len(cat.Tools) != 1 || cat.Tools[0].Name != "echo" || cat.Tools[0].Fingerprint == "" {
		t.Fatalf("catalog = %s", out)
	}
}

func TestMCPToolsOnlyReachConfiguredServers(t *testing.T) {
	specs, err := saigemcp.ParseConfig([]byte(`{"mcpServers": {"up": {"type": "http", "url": "http://127.0.0.1:1/mcp"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	tools := mcpTools(specs)
	for _, name := range []string{"mcp_probe", "mcp_catalog"} {
		_, err := toolNamed(t, tools, name).Execute(context.Background(), map[string]any{"server": "http://169.254.169.254/"})
		if err == nil || !strings.Contains(err.Error(), "unknown server") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// The probe of an unreachable server is a result, not an error.
	out, err := runTool(t, toolNamed(t, tools, "mcp_probe"), `{}`)
	if err != nil || !strings.Contains(out, `"ok": false`) {
		t.Fatalf("probe = %s, err %v", out, err)
	}
}
