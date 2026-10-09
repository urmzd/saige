package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/tui"
	"github.com/urmzd/saige/agent/types"
)

func TestBaseURLAppliesOnPresetPath(t *testing.T) {
	home, _ := catalogSandbox(t)
	clearProviderEnv(t)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	file := writeFile(t, filepath.Join(home, "c.json"), `{"version":1,"presets":{
		"solo":{"chain":[{"id":"o","provider":"openai","model":"gpt-4.1"}]},
		"derived":{"extends":"solo","description":"inherits the chain"},
		"duo":{"chain":[{"id":"a","provider":"anthropic","model":"claude-3-5-haiku"},{"id":"o","provider":"openai","model":"gpt-4.1"}]}}}`)
	build := func(presetName, prov string) (map[string]string, error) {
		cf := newTestFlags(prov, "", "", "")
		*cf.catalogs = []string{file}
		*cf.preset = presetName
		*cf.baseURL = "https://proxy.example/v1"
		resetCatalogCache()
		b, err := resolveBundle(context.Background(), cf, false)
		if err != nil {
			return nil, err
		}
		defer func() { _ = b.Close() }()
		rp, _ := b.Resolved(presetName)
		urls := map[string]string{}
		for _, e := range rp.Chain {
			urls[e.ID] = e.BaseURL
		}
		return urls, nil
	}
	for _, name := range []string{"solo", "derived"} {
		urls, err := build(name, "")
		if err != nil || urls["o"] != "https://proxy.example/v1" {
			t.Fatalf("%s: %v %v", name, urls, err)
		}
	}
	if _, err := build("duo", ""); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("multi-vendor chain without --provider: %v", err)
	}
	urls, err := build("duo", providerOpenAI)
	if err != nil || urls["o"] != "https://proxy.example/v1" || urls["a"] != "" {
		t.Fatalf("--provider openai: %v %v", urls, err)
	}
	if _, err := build("solo", providerGoogle); err == nil {
		t.Fatal("--provider naming no entry must fail")
	}
}

// ollamaServer answers the reachability probe.
func ollamaServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"0.0.0"}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestDefaultPresetIsSingleVendor(t *testing.T) {
	catalogSandbox(t)
	clearProviderEnv(t)
	t.Setenv("GEMINI_API_KEY", "")
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()

	run := func(host string) ([]string, error) {
		cf := newTestFlags("", "", "", "")
		*cf.ollamaHost = host
		resetCatalogCache()
		b, err := resolveBundle(context.Background(), cf, false)
		if err != nil {
			return nil, err
		}
		defer func() { _ = b.Close() }()
		rp, _ := b.Resolved("default")
		var ids []string
		for _, e := range rp.Chain {
			ids = append(ids, e.ProfileID)
		}
		return ids, nil
	}

	_, err := run(down.URL)
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY or OPENAI_API_KEY") || !strings.Contains(err.Error(), "start ollama") {
		t.Fatalf("no provider: %v", err)
	}
	if ids, err := run(ollamaServer(t)); err != nil || strings.Join(ids, ",") != "default/ollama" {
		t.Fatalf("local only: %v %v", ids, err)
	}
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	if ids, err := run(down.URL); err != nil || strings.Join(ids, ",") != "default/anthropic" {
		t.Fatalf("first vendor with a key: %v %v", ids, err)
	}
	// Naming the preset opts into its cross-vendor chain.
	cf := newTestFlags("", "", "", "")
	*cf.preset = "default"
	*cf.ollamaHost = down.URL
	resetCatalogCache()
	b, err := resolveBundle(context.Background(), cf, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	if rp, _ := b.Resolved("default"); len(rp.Chain) != 2 {
		t.Fatalf("explicit preset chain %+v", rp.Chain)
	}
}

func TestCatalogFlagNamingProjectFileLoadsOnce(t *testing.T) {
	_, project := catalogSandbox(t)
	proj := writeFile(t, filepath.Join(project, ".saige", "catalog.json"), `{"version":1,"revision":"project"}`)
	// A path spelled differently but naming the same file.
	alias := filepath.Join(project, "sub", "..", ".saige", "catalog.json")
	layers, err := discoverLayers([]string{alias}, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, l := range layers {
		kinds = append(kinds, l.Kind)
	}
	if strings.Join(kinds, ",") != "embedded,flag" || !layers[1].Trusted {
		t.Fatalf("layers %+v", layers)
	}
	cat, err := mergeLayers(context.Background(), layers)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(cat.Revision, "project") != 1 {
		t.Fatalf("revision %q: project file %s loaded more than once", cat.Revision, proj)
	}
}

func TestFailedAskPrintsErrorOnce(t *testing.T) {
	provider := &agenttest.ScriptedProvider{Responses: [][]types.Delta{{types.ErrorDelta{Error: errors.New("upstream exploded")}}}}
	ag := agentsdk.NewAgent(agentsdk.AgentConfig{Provider: provider})
	var stdout, stderr bytes.Buffer
	out := tui.ResolveOutputWriters(false, tui.TemplateDefault, &stdout, &stderr)
	err := runAsk(context.Background(), ag, "hi", out, false)
	if err == nil {
		t.Fatal("want the provider error")
	}
	// run prints nothing more for an error the command reported.
	var rep reportedError
	if !errors.As(reported(out, err), &rep) {
		t.Fatalf("not marked as reported: %v", err)
	}
	all := stdout.String() + stderr.String()
	if strings.Count(all, "upstream exploded") != 1 || !strings.Contains(stderr.String(), "upstream exploded") {
		t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
	}
}
