package definition

import (
	"fmt"
	"strings"
)

// MCPGroupPrefix marks a tool grant that stands for an MCP server's tools:
// "mcp:<server>" grants its read tools, "mcp:<server>:write" its write
// tools too. Compile writes these from tools.mcp and Policy.MCPWrite.
const MCPGroupPrefix = "mcp:"

// Classes an MCP group grant may end in.
const (
	MCPClassRead  = "read"
	MCPClassWrite = "write"
)

// MCPGrant is a parsed tool group grant.
type MCPGrant struct {
	// Provider is the MCP tool provider whose accepted tools the grant
	// stands for.
	Provider string
	// Write admits the provider's write tools as well as its read tools.
	Write bool
}

// ParseMCPGrant reports whether a tool grant is an MCP group grant and, if it
// is, parses it. ok is false for an ordinary tool name. err (ErrInvalid) is
// set for a grant that starts with MCPGroupPrefix but names no group.
//
// The class is the last colon-separated segment, and only when it is "read"
// or "write"; anything else is part of the group's name. Leaving the class
// off can only grant less.
func ParseMCPGrant(name string) (g MCPGrant, ok bool, err error) {
	rest, ok := strings.CutPrefix(name, MCPGroupPrefix)
	if !ok {
		return MCPGrant{}, false, nil
	}
	g.Provider = rest
	if i := strings.LastIndexByte(rest, ':'); i >= 0 {
		switch rest[i+1:] {
		case MCPClassWrite:
			g = MCPGrant{Provider: rest[:i], Write: true}
		case MCPClassRead:
			g = MCPGrant{Provider: rest[:i]}
		}
	}
	if g.Provider == "" || strings.TrimSpace(g.Provider) != g.Provider {
		return MCPGrant{}, true, fmt.Errorf("%w: tool group %q names no group", ErrInvalid, name)
	}
	return g, true, nil
}

// String is the grant in its canonical form.
func (g MCPGrant) String() string {
	if g.Write {
		return MCPGroupPrefix + g.Provider + ":" + MCPClassWrite
	}
	return MCPGroupPrefix + g.Provider
}
