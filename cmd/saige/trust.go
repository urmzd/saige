package main

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/urmzd/saige/agent/provider/catalog"
)

// untrustedFields is the allowlist an untrusted catalog layer (a project file
// that arrived with a cloned repository) is checked against. For every
// catalog type it lists each JSON field and whether such a layer may set it.
// A field or type missing from the table is refused, so a field added to the
// file format stays closed to untrusted layers until someone decides here.
//
// Refused fields could send a request, a credential, or a refused prompt
// somewhere the user did not choose:
//   - base_url and api_key_env point an entry at another host or credential;
//   - server_tools[].mcp_server makes the provider connect to a server;
//   - routing.failover_on_content_filter and routing.failover_on_auth send a
//     refused prompt or a failed credential's request to another vendor;
//   - inherit_default false discards the trusted layers below;
//   - vertex.project and vertex.location bill and send prompts to a Google
//     Cloud project the user did not choose, with the user's own
//     Application Default Credentials;
//   - dials are closed until their effect on untrusted layers is decided:
//     a dial can turn on prompt caching or change reasoning spend.
var untrustedFields = map[reflect.Type]map[string]bool{
	reflect.TypeFor[catalog.Catalog](): {
		"$schema": true, "version": true, "revision": true, "templates": true, "models": true,
		"baselines": true, "presets": true, "default_preset": true,
		"inherit_default": false, "dials": false,
	},
	reflect.TypeFor[catalog.ModelSpec](): {
		"provider": true, "prefix": true, "extends": true, "tier": true, "superseded_by": true,
		"capabilities": true, "add_capabilities": true, "remove_capabilities": true, "limits": true,
		"reasoning": true, "structured_output": true, "media": true, "server_tools": true,
		"server_tool_fees": true, "pricing": true, "defaults": true, "notes": true,
		"$replace": true, "$delete": true, "chat_completions_tools": true,
		"dials": false,
	},
	reflect.TypeFor[catalog.LimitsSpec](): {
		"context_window": true, "max_output_tokens": true, "default_max_output_tokens": true,
	},
	reflect.TypeFor[catalog.ReasoningSpec](): {
		"efforts": true, "default_effort": true, "required": true, "default_enabled": true,
		"min_budget": true, "max_budget": true, "dynamic_budget": true, "zero_budget": true,
		"sampling_requires_no_reasoning": true, "forced_tool_choice": true,
	},
	reflect.TypeFor[catalog.PricingSpec](): {
		"currency": true, "input_per_mtok": true, "output_per_mtok": true, "cached_input_per_mtok": true,
		"cache_write_per_mtok": true, "per_request": true, "free": true, "as_of": true, "source": true,
	},
	reflect.TypeFor[catalog.Fee](): {"currency": true, "per_use": true, "as_of": true, "source": true},
	reflect.TypeFor[catalog.OptionsSpec](): {
		"temperature": true, "top_p": true, "top_k": true, "frequency_penalty": true, "presence_penalty": true,
		"seed": true, "max_output_tokens": true, "stop": true, "parallel_tools": true, "reasoning": true,
		"tool_choice": true, "prompt_cache": true, "server_tools": true,
	},
	reflect.TypeFor[catalog.ReasoningOption](): {"enabled": true, "effort": true, "budget": true},
	reflect.TypeFor[catalog.PromptCacheSpec](): {
		"mode": true, "ttl": true, "tools": true, "system": true, "conversation": true, "retention": true, "key": true,
	},
	reflect.TypeFor[catalog.ServerToolSpec](): {
		"kind": true, "max_uses": true, "allowed_domains": true, "blocked_domains": true, "user_location": true,
		"mcp_server": false,
	},
	// Unreachable while mcp_server itself is refused; listed so every field
	// of the file format has a decision.
	reflect.TypeFor[catalog.MCPServerSpec](): {
		"name": false, "url": false, "allowed_tools": false, "require_approval": false,
	},
	reflect.TypeFor[catalog.PresetSpec](): {
		"description": true, "extends": true, "options": true, "tool_choice": true, "output_mode": true,
		"llm_timeout": true, "retry": true, "routing": true, "require_declared": true, "chain": true,
		"dials": false, "compaction": false,
	},
	// Unreachable while compaction itself is refused: a repository layer
	// must not choose what the agent forgets or which model it pays to
	// summarize with.
	reflect.TypeFor[catalog.CompactionSpec](): {
		"strategy": false, "max_input_tokens": false, "target_tokens": false, "keep_turns": false,
		"select_k": false, "threshold": false, "keep_last": false, "window_size": false,
		"keep_tool_results": false, "exclude_tools": false, "summary_model": false, "chain": false,
	},
	reflect.TypeFor[catalog.RetrySpec](): {
		"max_attempts": true, "base_delay": true, "max_delay": true, "multiplier": true,
		"max_retry_after": true, "disable": true,
	},
	reflect.TypeFor[catalog.RoutingSpec](): {
		"policy": true, "fail_threshold": true, "reprobe_after": true, "required": true,
		"failover_on_content_filter": false, "failover_on_auth": false,
	},
	// local_fallback only picks among models already pulled on the entry's
	// own server, so it widens nothing an untrusted layer could not name.
	reflect.TypeFor[catalog.EntrySpec](): {
		"id": true, "provider": true, "model": true, "options": true, "unset": true, "inherit": true,
		"retry": true, "attempt_timeout": true, "optional": true, "local_fallback": true,
		"base_url": false, "api_key_env": false, "vertex": true, "dials": false,
	},
	// An untrusted layer may ask for Vertex on an entry, but not name the
	// project or location it bills and sends prompts to.
	reflect.TypeFor[catalog.VertexSpec](): {"project": false, "location": false},
}

// untrustedIssues reports every value an untrusted layer sets outside the
// allowlist, with its path in the file.
func untrustedIssues(c *catalog.Catalog) []catalog.Issue {
	var out []catalog.Issue
	walkUntrusted(reflect.ValueOf(c), "", &out)
	return out
}

func walkUntrusted(v reflect.Value, path string, out *[]catalog.Issue) {
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Struct:
		allowed, known := untrustedFields[v.Type()]
		t := v.Type()
		for i := range t.NumField() {
			f := t.Field(i)
			if !f.IsExported() || v.Field(i).IsZero() {
				continue
			}
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "" || name == "-" {
				continue
			}
			child := name
			if path != "" {
				child = path + "." + name
			}
			if ok := allowed[name]; !known || !ok {
				*out = append(*out, catalog.Issue{Path: child, Code: catalog.CodeUntrusted, Severity: catalog.SeverityError,
					Message: fmt.Sprintf("a project catalog may not set %s; set %s=1 or name the file with --catalog to trust it", name, envTrustProject)})
				continue
			}
			walkUntrusted(v.Field(i), child, out)
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			walkUntrusted(v.Index(i), fmt.Sprintf("%s[%d]", path, i), out)
		}
	case reflect.Map:
		keys := v.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return fmt.Sprint(keys[i]) < fmt.Sprint(keys[j]) })
		for _, k := range keys {
			walkUntrusted(v.MapIndex(k), fmt.Sprintf("%s.%v", path, k), out)
		}
	}
}
