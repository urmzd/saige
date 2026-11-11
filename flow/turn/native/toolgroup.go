package native

import (
	"fmt"
	"strings"

	saigetypes "github.com/urmzd/saige/agent/types"
)

// MCPGroupPrefix marks an entry of Spec.Tools as a tool group: "mcp:urmzd"
// grants the read tools of the MCP provider named urmzd as one name, and
// "mcp:urmzd:write" its read and write tools.
const MCPGroupPrefix = "mcp:"

// MCPGroup returns the grant name of an MCP provider's read-only tool group.
func MCPGroup(provider string) string { return MCPGroupPrefix + provider }

// MCPWriteGroup returns the grant name of an MCP provider's tool group with
// its write tools as well as its read tools.
func MCPWriteGroup(provider string) string { return MCPGroupPrefix + provider + ":write" }

// IsToolGroup reports whether a Spec.Tools entry names a tool group rather
// than a tool.
func IsToolGroup(name string) bool { return strings.HasPrefix(name, MCPGroupPrefix) }

// ErrNoSuchToolGroup reports a spec granting a group this deployment cannot
// resolve, or one that holds no tools. Callers match with errors.Is; it is
// also ErrInvalid.
var ErrNoSuchToolGroup = fmt.Errorf("%w: no such tool group", ErrInvalid)

// errMalformedGroup is a group whose members cannot be registered: a bug in
// whatever built it, which always fails the build rather than being dropped
// as unavailable.
var errMalformedGroup = fmt.Errorf("%w: malformed tool group", ErrInvalid)

// groupMembers returns the prefix every member of an MCP group's tools
// carries, "<provider>__", so an approval naming a member of a group that
// was dropped can be recognised. Empty for a name that is not an MCP group.
func groupMembers(name string) string {
	provider := strings.TrimPrefix(name, MCPGroupPrefix)
	if provider == name {
		return ""
	}
	provider = strings.TrimSuffix(strings.TrimSuffix(provider, ":write"), ":read")
	if provider == "" {
		return ""
	}
	return provider + "__"
}

// ToolGroup is a set of tools a spec grants under one name: an MCP
// provider's callable tools, as pkg/mcptools builds them for one grant.
//
// The runner registers the members with the model, each under its own name,
// and never the group: there is no tool called "mcp:urmzd" for a model to
// call. That keeps approvals, tool events and usage about the tool that ran
// ("urmzd__create_issue"), which is the thing a person can judge.
//
// The wiring from pkg/mcptools lives with the host because this package does
// not depend on MCP. pkg/runtime's WithToolGroupSource with an
// *mcptools.Grants supplies every "mcp:" group from the tool provider store,
// per tenant; a host with a single session can also build one by hand:
//
//	ts, err := session.Toolset(ctx, snapshot, toolprovider.GrantRead)
//	group := native.ToolGroup{Name: native.MCPGroup(provider.Name), Tools: ts.All()}
//
// The read/write class is part of the group's name: "mcp:<provider>" is read
// only and "mcp:<provider>:write" admits write tools too, and whoever builds
// the group builds its members under that class. A spec granting both gets
// the union, which is the write group: a read tool behaves the same under
// either.
//
// # When a group cannot be expanded
//
// A group this process cannot expand right now (no such provider for the
// tenant, the server unreachable, nothing the grant's class admits) is
// dropped like a single unavailable tool: the run continues without its
// tools, the model is told the group is unavailable, the operator is logged
// why, and Runner.Unavailable lists it. WithStrictTools refuses the build
// instead. A group with a malformed member is a bug and always refuses.
//
// # When a group is expanded
//
// A group is expanded when a Runner is built, exactly as a single tool name
// is resolved then. The spec, its hash and its pinned revision record the
// group, not its members, so a tool an admin accepts on the provider reaches
// every agent granted the group on its next run without a new revision. That
// is the point of a group, and also its cost: tool definitions are the first
// bytes of the cached prefix, so accepting a tool invalidates the prompt cache
// of those agents once. Grant tools one by one when the set must not move.
type ToolGroup struct {
	Name  string
	Tools []saigetypes.Tool
}

// ToolGroups is the optional face of a Tools catalog that also resolves tool
// groups. A catalog built per turn implements it to hand a turn the groups of
// that turn's tenant.
type ToolGroups interface {
	ToolGroup(name string) (ToolGroup, error)
}

// WithToolGroups registers groups a spec may grant by name. A group
// registered here wins over one the Tools catalog resolves.
func WithToolGroups(groups ...ToolGroup) Option {
	return func(c *config) {
		for _, g := range groups {
			if g.Name == "" {
				continue
			}
			if c.groups == nil {
				c.groups = map[string]ToolGroup{}
			}
			c.groups[g.Name] = g
		}
	}
}

// group resolves a granted group from the registered groups, then from the
// catalog. A group with no tools is refused: a grant that grants nothing is a
// mistake to report, not an empty set to accept.
func (c *config) group(rev Revision, name string) (ToolGroup, error) {
	g, ok := c.groups[name]
	if !ok {
		groups, can := c.tools.(ToolGroups)
		if !can {
			return ToolGroup{}, fmt.Errorf("%w: agent %q version %s grants %q, which this deployment has not registered",
				ErrNoSuchToolGroup, rev.Spec.Name, rev.Version, name)
		}
		var err error
		if g, err = groups.ToolGroup(name); err != nil {
			return ToolGroup{}, fmt.Errorf("native: agent %q version %s: %w", rev.Spec.Name, rev.Version, err)
		}
	}
	if len(g.Tools) == 0 {
		return ToolGroup{}, fmt.Errorf("%w: agent %q version %s grants %q, which holds no tools",
			ErrNoSuchToolGroup, rev.Spec.Name, rev.Version, name)
	}
	for _, t := range g.Tools {
		if n := t.Definition().Name; n == "" || IsToolGroup(n) {
			return ToolGroup{}, fmt.Errorf("%w: group %q has a member named %q", errMalformedGroup, name, n)
		}
	}
	return g, nil
}
