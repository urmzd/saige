package native_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	saigetypes "github.com/urmzd/saige/agent/types"

	"github.com/urmzd/saige/flow/turn/native"
)

// mcpTool stands in for a pkg/mcptools callable: a tool named
// "<provider>__<tool>".
type mcpTool struct{ name string }

func (t mcpTool) Definition() saigetypes.ToolDef {
	return saigetypes.ToolDef{Name: t.name, Parameters: saigetypes.ParameterSchema{Type: "object"}}
}
func (t mcpTool) Execute(context.Context, map[string]any) (string, error) {
	return "ran " + t.name, nil
}

func group(provider string, tools ...string) native.ToolGroup {
	g := native.ToolGroup{Name: native.MCPGroup(provider)}
	for _, n := range tools {
		g.Tools = append(g.Tools, mcpTool{name: n})
	}
	return g
}

// groupCatalog resolves single tools from a fixed set and groups from a map,
// like a per-turn catalog serving a tenant's MCP providers.
type groupCatalog struct {
	native.Tools
	groups map[string]native.ToolGroup
}

func (c groupCatalog) ToolGroup(name string) (native.ToolGroup, error) {
	g, ok := c.groups[name]
	if !ok {
		return native.ToolGroup{}, fmt.Errorf("%w: %q", native.ErrNoSuchToolGroup, name)
	}
	return g, nil
}

// toolsShown builds a runner, runs one turn, and returns the tool names the
// model was shown.
func toolsShown(t *testing.T, tools, approvals []string, opts ...native.Option) ([]string, error) {
	t.Helper()
	var shown []string
	spy := func(native.Spec) (saigetypes.Provider, error) {
		return providerFunc(func(_ context.Context, _ []saigetypes.Message, defs []saigetypes.ToolDef) (<-chan saigetypes.Delta, error) {
			for _, d := range defs {
				shown = append(shown, d.Name)
			}
			return scripted(agenttest.TextResponse("ok")), nil
		}), nil
	}
	rev := offlineRevision(1, "You help.")
	rev.Spec.Provider = "spy"
	rev.Spec.Tools = tools
	rev.Spec.Approvals = approvals
	r, err := native.New(rev, append([]native.Option{native.WithProvider("spy", spy)}, opts...)...)
	if err != nil {
		return nil, err
	}
	collect(t, r, native.Turn{Text: "go"})
	slices.Sort(shown)
	return shown, nil
}

// The model is shown the group's members, each under its own name, and never
// a tool named after the group.
func TestAnMCPGroupRegistersItsMembersNeverItself(t *testing.T) {
	t.Parallel()
	urmzd := group("urmzd", "urmzd__create_issue", "urmzd__search")
	docs := group("docs", "docs__lookup")
	catalog := groupCatalog{
		Tools:  native.StaticTools(mcpTool{name: "fetch"}),
		groups: map[string]native.ToolGroup{docs.Name: docs},
	}

	tests := []struct {
		name  string
		tools []string
		opts  []native.Option
		want  []string
	}{
		{name: "a registered group", tools: []string{"mcp:urmzd"},
			opts: []native.Option{native.WithToolGroups(urmzd)},
			want: []string{"urmzd__create_issue", "urmzd__search"}},
		{name: "a group the catalog resolves", tools: []string{"mcp:docs"},
			opts: []native.Option{native.WithTools(catalog)},
			want: []string{"docs__lookup"}},
		{name: "groups beside a single tool", tools: []string{"fetch", "mcp:docs", "mcp:urmzd"},
			opts: []native.Option{native.WithTools(catalog), native.WithToolGroups(urmzd)},
			want: []string{"docs__lookup", "fetch", "urmzd__create_issue", "urmzd__search"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			shown, err := toolsShown(t, tt.tools, nil, tt.opts...)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if !slices.Equal(shown, tt.want) {
				t.Errorf("model was shown %v, want %v", shown, tt.want)
			}
		})
	}
}

// A group this deployment cannot expand costs the agent those tools, not the
// run: the rest is still shown, the runner reports the group, and the model
// is told it is unavailable without being told why. Strict tools refuse.
func TestAnMCPGroupThatCannotBeExpandedIsDroppedWithAWarning(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		opts []native.Option
	}{
		{name: "not registered and no catalog"},
		{name: "a catalog without groups", opts: []native.Option{native.WithTools(native.StaticTools(mcpTool{name: "fetch"}))}},
		{name: "a catalog that does not know it", opts: []native.Option{native.WithTools(groupCatalog{Tools: native.StaticTools(mcpTool{name: "fetch"})})}},
		{name: "a group with no tools", opts: []native.Option{native.WithTools(native.StaticTools(mcpTool{name: "fetch"})), native.WithToolGroups(group("urmzd"))}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rev := offlineRevision(1, "You help.")
			rev.Spec.Tools = []string{"fetch", "mcp:urmzd"}
			r, err := native.New(rev, tt.opts...)
			if err != nil {
				t.Fatalf("New: %v, want the group dropped", err)
			}
			gone := r.Unavailable()
			if !slices.ContainsFunc(gone, func(u native.UnavailableTool) bool { return u.Name == "mcp:urmzd" && u.Reason != "" }) {
				t.Fatalf("Unavailable = %+v, want mcp:urmzd with its reason", gone)
			}

			only := offlineRevision(1, "You help.")
			only.Spec.Tools = []string{"mcp:urmzd"}
			strict := append([]native.Option{native.WithStrictTools()}, tt.opts...)
			if _, err := native.New(only, strict...); !errors.Is(err, native.ErrNoSuchToolGroup) {
				t.Fatalf("strict New: %v, want ErrNoSuchToolGroup", err)
			}
		})
	}

	// The model is told the group's name, never the reason.
	var system string
	spy := func(native.Spec) (saigetypes.Provider, error) {
		return providerFunc(func(_ context.Context, msgs []saigetypes.Message, _ []saigetypes.ToolDef) (<-chan saigetypes.Delta, error) {
			for _, m := range msgs {
				if _, ok := m.(saigetypes.SystemMessage); ok {
					system += renderMessages([]saigetypes.Message{m})
				}
			}
			return scripted(agenttest.TextResponse("ok")), nil
		}), nil
	}
	rev := offlineRevision(1, "You help.")
	rev.Spec.Provider = "spy"
	rev.Spec.Tools = []string{"mcp:urmzd"}
	r, err := native.New(rev, native.WithProvider("spy", spy))
	if err != nil {
		t.Fatal(err)
	}
	collect(t, r, native.Turn{Text: "go"})
	if !strings.Contains(system, "mcp:urmzd") || strings.Contains(system, "not registered") {
		t.Fatalf("system prompt %q: want the group named, without the reason", system)
	}
}

// An approval on a member of a dropped group has nothing left to guard, so it
// does not refuse the build; one on a tool no grant covers still does.
func TestAnApprovalOnADroppedGroupsMemberIsSkipped(t *testing.T) {
	t.Parallel()
	for _, grant := range []string{"mcp:urmzd", "mcp:urmzd:write"} {
		if _, err := toolsShown(t, []string{grant}, []string{"urmzd__create_issue"}); err != nil {
			t.Fatalf("%s: New: %v, want the approval skipped with its group", grant, err)
		}
	}
	if _, err := toolsShown(t, []string{"mcp:urmzd"}, []string{"other__delete"}); !errors.Is(err, native.ErrInvalid) {
		t.Fatalf("an approval no grant covers: %v, want ErrInvalid", err)
	}
}

// A group with a malformed member is a bug in what built it, so it refuses to
// build even though an unavailable group would be dropped.
func TestAMalformedMCPGroupAlwaysRefuses(t *testing.T) {
	t.Parallel()
	bad := native.ToolGroup{Name: "mcp:urmzd", Tools: []saigetypes.Tool{mcpTool{name: "mcp:nested"}}}
	if _, err := toolsShown(t, []string{"mcp:urmzd"}, nil, native.WithToolGroups(bad)); !errors.Is(err, native.ErrInvalid) {
		t.Fatalf("New: %v, want ErrInvalid", err)
	}
}

// An approval on a member is enforced, and one on a tool the group does not
// hold refuses to build: the guard never silently lapses.
func TestApprovalsHoldForToolsGrantedThroughAnMCPGroup(t *testing.T) {
	t.Parallel()
	urmzd := native.WithToolGroups(group("urmzd", "urmzd__create_issue", "urmzd__search"))

	if _, err := toolsShown(t, []string{"mcp:urmzd"}, []string{"urmzd__create_issue"}, urmzd); err != nil {
		t.Errorf("approval for a member: New: %v", err)
	}
	if _, err := toolsShown(t, []string{"mcp:urmzd"}, []string{"urmzd__delete_repo"}, urmzd); !errors.Is(err, native.ErrInvalid) {
		t.Errorf("approval for a tool outside the group: New: %v, want ErrInvalid", err)
	}
}
