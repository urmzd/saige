package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCatalogMigrate(t *testing.T) {
	_, project := catalogSandbox(t)
	v1 := writeFile(t, filepath.Join(project, "v1.json"), `{"version":1,"revision":"r","models":[
		{"provider":"openai","prefix":"gpt-x","extends":"openai.chat","pricing":{"input_per_mtok":1,"batch_discount":0.5,"as_of":"2026-10-09"}}]}`)
	code, out := runCLI(t, "catalog", "migrate", v1)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var doc struct {
		Version   int                        `json:"version"`
		Models    map[string]json.RawMessage `json:"models"`
		Offerings []struct {
			Model, Endpoint string
			Tiers           map[string]json.RawMessage
		} `json:"offerings"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if doc.Version != 2 || doc.Models["openai/gpt-x"] == nil || len(doc.Offerings) != 1 ||
		doc.Offerings[0].Endpoint != "openai-chat" || doc.Offerings[0].Tiers["batch"] == nil {
		t.Fatalf("migrated = %s", out)
	}
	if code, out := runCLI(t, "catalog", "migrate", "--write", v1); code != 0 {
		t.Fatalf("write: exit %d: %s", code, out)
	}
	raw, _ := os.ReadFile(v1)
	if !strings.Contains(string(raw), `"version": 2`) {
		t.Fatalf("file not rewritten: %s", raw)
	}
}

func TestCatalogShowNamesOfferings(t *testing.T) {
	catalogSandbox(t)
	code, out := runCLI(t, "catalog", "show", "openai/gpt-6-luna")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !strings.Contains(out, "endpoint openai-chat") || !strings.Contains(out, "offering openai/gpt-6-luna@openai-chat") {
		t.Fatalf("show does not name the offering:\n%s", out)
	}
}
