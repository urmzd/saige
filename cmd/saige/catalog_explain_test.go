package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestCatalogExplainHuman(t *testing.T) {
	code, out := runCLI(t, "catalog", "explain", "default", "--dials", `{"creativity":"focused","reasoning":{"depth":"high"}}`)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	for _, want := range []string{
		"anthropic/claude-haiku-5-5", "creativity  focused    dropped", "the model takes no sampling controls",
		"conflicts with effort high", "ollama/qwen3", "depth collapsed to toggle", "sends: temperature=0.3 top_p=0.9",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestCatalogExplainJSONAndToolsSurface(t *testing.T) {
	code, out := runCLI(t, "--format", "json", "catalog", "explain", "openai/gpt-6-luna",
		"--dials", `{"reasoning":{"depth":"high"}}`, "--tools")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var got explainedPreset
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	e := got.Chain[0]
	if e.Surface != types.SurfaceChat || len(e.Decisions) != 1 || e.Decisions[0].Action != types.DialMapped || e.Effective["reasoning"] != "effort none" {
		t.Fatalf("chat with tools: %+v", e)
	}
	code, out = runCLI(t, "--format", "json", "catalog", "explain", "openai/gpt-6-luna",
		"--dials", `{"reasoning":{"depth":"high"}}`, "--tools", "--surface", "responses")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	got = explainedPreset{}
	_ = json.Unmarshal([]byte(out), &got)
	if e := got.Chain[0]; e.Effective["reasoning"] != "effort high" || e.EffectiveHash == "" {
		t.Fatalf("responses: %+v", e)
	}
}

// A contractual dial an entry cannot honor is reported as rejected with the
// reason, and the other entries still compile.
func TestCatalogExplainRejectionAndPolicy(t *testing.T) {
	file := filepath.Join(t.TempDir(), "catalog.json")
	doc := `{"version":1,"presets":{"duo":{"chain":[{"provider":"openai","model":"gpt-4.1"},{"provider":"anthropic","model":"claude-haiku-5-5"}]}}}`
	if err := os.WriteFile(file, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := runCLI(t, "--catalog", file, "--format", "json", "catalog", "explain", "duo", "--dials", `{"reproducible":7}`)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var got explainedPreset
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got.Chain[0].Error != "" || got.Chain[0].Decisions[0].Action != types.DialApplied {
		t.Fatalf("gpt-4.1: %+v", got.Chain[0])
	}
	if h := got.Chain[1]; h.Error == "" || h.Decisions[0].Action != types.DialRejected || !strings.Contains(h.Decisions[0].Reason, "seed") {
		t.Fatalf("haiku: %+v", h)
	}
	code, out = runCLI(t, "--catalog", file, "catalog", "explain", "duo", "--dials", `{"reproducible":7}`, "--policy", "reproducible=drop")
	if code != 0 || !strings.Contains(out, "policy default reproducible=drop") || strings.Contains(out, "error:") {
		t.Fatalf("loosened: %d %s", code, out)
	}
	if code, out = runCLI(t, "catalog", "explain", "default", "--dials", `{"creativity":"wild"}`); code == 0 {
		t.Fatalf("an invalid dial value was accepted: %s", out)
	}
	if code, out = runCLI(t, "catalog", "explain", "default", "--dials", `{"temperature":1}`); code == 0 {
		t.Fatalf("an unknown dial was accepted: %s", out)
	}
}
