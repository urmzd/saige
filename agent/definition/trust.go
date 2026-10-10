package definition

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/types"
)

// UntrustedFields is the allowlist an untrusted definition (a project file
// that arrived with a cloned repository) is checked against. For each type
// of the file format it lists every JSON field and whether such a
// definition may set it. A field or type missing from the table is
// refused, so a field added to the format stays closed to untrusted
// definitions until someone decides here.
//
// Refused fields could send traffic, credentials or data somewhere the user
// did not choose, or loosen what a person must approve:
//   - tools.mcp connects to servers;
//   - memory reads and writes the user's durable memory;
//   - approval.allow, approval.capabilities and approval.ramp_after let
//     calls run without asking; approval.ask, approval.deny,
//     approval.deny_after and approval.grant only tighten, and are allowed;
//   - dials and compaction are closed as they are for an untrusted catalog
//     layer: they change spend and what the agent forgets.
//
// Harness groups are checked by value too: an untrusted definition may ask
// for read and write, but not exec, which runs code, or web, which can
// send workspace contents to any host.
var UntrustedFields = map[reflect.Type]map[string]bool{
	reflect.TypeFor[Definition](): {
		"apiVersion": true, "name": true, "version": true, "description": true, "model": true,
		"tools": true, "skills": true, "subagents": true, "approval": true, "guardrails": true,
		"limits": true, "metadata": true,
		"dials": false, "memory": false, "compaction": false,
	},
	reflect.TypeFor[ModelRef]():    {"use": true, "fallback": true},
	reflect.TypeFor[ToolsSpec]():   {"harness": true, "registry": true, "mcp": false},
	reflect.TypeFor[SkillRef]():    {"name": true, "mode": true, "triggers": true, "hash": true},
	reflect.TypeFor[SubagentRef](): {"ref": true, "mode": true, "description": true, "budget": true},
	reflect.TypeFor[SubagentBudget](): {
		"max_iterations": true, "timeout": true, "max_cost": true, "max_tokens": true, "max_requests": true,
	},
	reflect.TypeFor[ApprovalSpec](): {
		"ask": true, "deny": true, "deny_after": true, "grant": true, "hide_denied": true,
		"allow": false, "capabilities": false, "ramp_after": false,
	},
	reflect.TypeFor[GuardrailsSpec](): {"input": true, "output": true},
	reflect.TypeFor[GuardrailSpec](): {
		"name": true, "redact": true, "max": true, "patterns": true, "policy": true, "parallel": true,
	},
	reflect.TypeFor[PatternSpec](): {"label": true, "expr": true},
	reflect.TypeFor[LimitsSpec]():  {"max_iterations": true, "llm_timeout": true, "tool_timeout": true, "budget": true},
	reflect.TypeFor[BudgetSpec](): {
		"max_cost": true, "max_tokens": true, "max_requests": true, "warn_at": true, "on_exceed": true,
		"allow_unpriced": false,
	},
	// Unreachable while their parents are refused; listed so every type of
	// the format has a decision.
	reflect.TypeFor[MCPRef]():                 {"server": false, "allow": false},
	reflect.TypeFor[MemorySpec]():             {},
	reflect.TypeFor[types.Dials]():            {},
	reflect.TypeFor[catalog.CompactionSpec](): {},
}

// untrustedHarness lists the harness groups an untrusted definition may
// name.
var untrustedHarness = map[string]bool{"read": true, "write": true}

func untrustedIssues(d *Definition) issues {
	var out issues
	walkUntrusted(reflect.ValueOf(d), "", &out)
	if d.Tools != nil {
		for i, g := range d.Tools.Harness {
			if !untrustedHarness[g] {
				out.add(fmt.Sprintf("tools.harness[%d]", i), 0, CodeUntrusted,
					"an untrusted definition may not use the %s harness group; trust its source to allow it", g)
			}
		}
	}
	return out
}

func walkUntrusted(v reflect.Value, path string, out *issues) {
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Struct:
		t := v.Type()
		allowed, known := UntrustedFields[t]
		if !known {
			out.add(path, 0, CodeUntrusted, "an untrusted definition may not set %s; trust its source to allow it", path)
			return
		}
		for i := range t.NumField() {
			f := t.Field(i)
			if !f.IsExported() || v.Field(i).IsZero() {
				continue
			}
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "" || name == "-" {
				continue
			}
			child := joinPath(path, name)
			if !allowed[name] {
				out.add(child, 0, CodeUntrusted, "an untrusted definition may not set %s; trust its source to allow it", name)
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
			walkUntrusted(v.MapIndex(k), joinPath(path, fmt.Sprint(k)), out)
		}
	}
}
