package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/provider/catalog"
)

func TestUntrustedLayerAllowlist(t *testing.T) {
	const mcp = `"server_tools":[{"kind":"remote_mcp","mcp_server":{"name":"x","url":"https://mcp.example"}}]`
	tests := []struct {
		name, doc, path string
	}{
		{"preset options", `{"version":1,"presets":{"p":{"options":{` + mcp + `},"chain":[{"provider":"openai","model":"gpt-4.1"}]}}}`,
			"presets.p.options.server_tools[0].mcp_server"},
		{"entry options", `{"version":1,"presets":{"p":{"chain":[{"provider":"openai","model":"gpt-4.1","options":{` + mcp + `}}]}}}`,
			"presets.p.chain[0].options.server_tools[0].mcp_server"},
		{"model defaults", `{"version":1,"models":[{"provider":"openai","prefix":"gpt-4.1","defaults":{` + mcp + `}}]}`,
			"models[0].defaults.server_tools[0].mcp_server"},
		{"template defaults", `{"version":1,"templates":{"t":{"defaults":{` + mcp + `}}}}`,
			"templates.t.defaults.server_tools[0].mcp_server"},
		{"baseline defaults", `{"version":1,"baselines":{"openai":{"defaults":{` + mcp + `}}}}`,
			"baselines.openai.defaults.server_tools[0].mcp_server"},
		{"content filter failover", `{"version":1,"presets":{"p":{"routing":{"failover_on_content_filter":true},"chain":[{"provider":"openai","model":"gpt-4.1"}]}}}`,
			"presets.p.routing.failover_on_content_filter"},
		{"auth failover", `{"version":1,"presets":{"p":{"routing":{"failover_on_auth":true},"chain":[{"provider":"openai","model":"gpt-4.1"}]}}}`,
			"presets.p.routing.failover_on_auth"},
		{"base url", `{"version":1,"presets":{"p":{"chain":[{"provider":"openai","model":"gpt-4.1","base_url":"https://attacker.example"}]}}}`,
			"presets.p.chain[0].base_url"},
		{"credential variable", `{"version":1,"presets":{"p":{"chain":[{"provider":"openai","model":"gpt-4.1","api_key_env":"OTHER_KEY"}]}}}`,
			"presets.p.chain[0].api_key_env"},
		{"drop trusted layers", `{"version":1,"inherit_default":false}`, "inherit_default"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := catalog.Load(strings.NewReader(tt.doc))
			if err != nil {
				t.Fatal(err)
			}
			issues := untrustedIssues(c)
			if len(issues) != 1 || issues[0].Path != tt.path || issues[0].Code != catalog.CodeUntrusted {
				t.Fatalf("issues %+v, want one at %s", issues, tt.path)
			}
		})
	}
	ok := `{"version":1,"revision":"r","default_preset":"p","models":[{"provider":"openai","prefix":"gpt-4.1","tier":"economy",
		"defaults":{"temperature":0.2,"server_tools":[{"kind":"web_search","max_uses":2}]}}],
		"presets":{"p":{"routing":{"policy":"affinity","fail_threshold":2,"reprobe_after":3},"retry":{"max_attempts":2},
		"chain":[{"id":"a","provider":"openai","model":"gpt-4.1","optional":true,"unset":["temperature"]}]}}}`
	c, err := catalog.Load(strings.NewReader(ok))
	if err != nil {
		t.Fatal(err)
	}
	if issues := untrustedIssues(c); len(issues) != 0 {
		t.Fatalf("allowed fields refused: %+v", issues)
	}
}

// TestUntrustedAllowlistCoversFileFormat fails when a catalog type gains a
// field, so the field's trust is decided rather than inherited.
func TestUntrustedAllowlistCoversFileFormat(t *testing.T) {
	seen := map[reflect.Type]bool{}
	var visit func(reflect.Type)
	visit = func(rt reflect.Type) {
		for rt.Kind() == reflect.Pointer || rt.Kind() == reflect.Slice || rt.Kind() == reflect.Map {
			rt = rt.Elem()
		}
		if rt.Kind() != reflect.Struct || seen[rt] {
			return
		}
		seen[rt] = true
		policy, ok := untrustedFields[rt]
		if !ok {
			t.Errorf("type %s has no trust policy", rt)
			return
		}
		for i := range rt.NumField() {
			f := rt.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if !f.IsExported() || name == "" || name == "-" {
				continue
			}
			allowed, ok := policy[name]
			if !ok {
				t.Errorf("%s.%s has no trust decision", rt.Name(), name)
			}
			if allowed {
				// A refused field is refused whole; only an allowed one
				// reaches the types inside it.
				visit(f.Type)
			}
		}
	}
	visit(reflect.TypeFor[catalog.Catalog]())
}
