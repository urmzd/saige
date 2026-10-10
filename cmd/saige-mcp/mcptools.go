package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	saigemcp "github.com/urmzd/saige/agent/mcp"
	agenttypes "github.com/urmzd/saige/agent/types"
)

// mcpToolTimeout bounds one probe or listing.
const mcpToolTimeout = 30 * time.Second

// mcpTools returns mcp_probe and mcp_catalog over the servers of an MCP
// configuration file. A caller can only name a configured server: the tools
// never connect to an address or start a command the caller supplies.
func mcpTools(specs []saigemcp.ServerSpec) []agenttypes.Tool {
	byName := make(map[string]saigemcp.ServerSpec, len(specs))
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		byName[s.Name] = s
		names = append(names, s.Name)
	}
	lookup := func(args map[string]any) (saigemcp.ServerSpec, error) {
		name, _ := args["server"].(string)
		spec, ok := byName[name]
		if !ok {
			return spec, fmt.Errorf("%w: unknown server %q (configured: %s)", agenttypes.ErrInvalidToolArguments, name, strings.Join(names, ", "))
		}
		return spec, nil
	}
	server := agenttypes.PropertyDef{Type: agenttypes.SchemaString, Description: "A configured server", Enum: names}

	probe := &agenttypes.ToolFunc{
		Def: agenttypes.ToolDef{
			Name: "mcp_probe",
			Description: "Health-check configured MCP servers: connect, list tools and disconnect. " +
				"Reports ok, unreachable, auth_rejected, blocked, timeout or protocol for each, with the tool count and latency. " +
				"Without a server, probes every configured server.",
			Capability: agenttypes.ToolCapabilityRead,
			Parameters: agenttypes.ParameterSchema{
				Type:       agenttypes.SchemaObject,
				Properties: map[string]agenttypes.PropertyDef{"server": server},
			},
		},
		Fn: func(ctx context.Context, args map[string]any) (string, error) {
			targets := specs
			if _, ok := args["server"]; ok {
				spec, err := lookup(args)
				if err != nil {
					return "", err
				}
				targets = []saigemcp.ServerSpec{spec}
			}
			type result struct {
				Server string `json:"server"`
				OK     bool   `json:"ok"`
				Kind   string `json:"kind"`
				// Message is the failure, empty on success.
				Message   string   `json:"message,omitempty"`
				ToolCount int      `json:"tool_count"`
				Tools     []string `json:"tools,omitempty"`
				LatencyMs int64    `json:"latency_ms"`
				Status    int      `json:"status,omitempty"`
			}
			out := make([]result, 0, len(targets))
			for _, spec := range targets {
				pctx, cancel := context.WithTimeout(ctx, mcpToolTimeout)
				r := saigemcp.Probe(pctx, spec)
				cancel()
				out = append(out, result{Server: spec.Name, OK: r.OK, Kind: string(r.Kind), Message: r.Message,
					ToolCount: r.ToolCount, Tools: r.Tools, LatencyMs: r.Latency.Milliseconds(), Status: r.Status})
			}
			return encodeJSON(out)
		},
	}

	catalog := &agenttypes.ToolFunc{
		Def: agenttypes.ToolDef{
			Name: "mcp_catalog",
			Description: "List the tools a configured MCP server advertises, exactly as advertised: name, description, " +
				"input schema, annotations and a fingerprint that changes when any of them does.",
			Capability: agenttypes.ToolCapabilityRead,
			Parameters: agenttypes.ParameterSchema{
				Type:       agenttypes.SchemaObject,
				Required:   []string{"server"},
				Properties: map[string]agenttypes.PropertyDef{"server": server},
			},
		},
		Fn: func(ctx context.Context, args map[string]any) (string, error) {
			spec, err := lookup(args)
			if err != nil {
				return "", err
			}
			cctx, cancel := context.WithTimeout(ctx, mcpToolTimeout)
			defer cancel()
			c, err := saigemcp.Connect(cctx, spec)
			if err != nil {
				return "", fmt.Errorf("connect to %s: %w", spec.Name, err)
			}
			defer func() { _ = c.Close(ctx) }()
			cat, err := c.Catalog(cctx)
			if err != nil {
				return "", fmt.Errorf("list tools of %s: %w", spec.Name, err)
			}
			return encodeJSON(cat)
		},
	}
	return []agenttypes.Tool{probe, catalog}
}
