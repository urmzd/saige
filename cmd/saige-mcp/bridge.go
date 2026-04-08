package main

import (
	"context"
	"encoding/json"

	agenttypes "github.com/urmzd/saige/agent/types"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerTool bridges a saige Tool into an MCP server.
func registerTool(server *mcp.Server, tool agenttypes.Tool) {
	def := tool.Definition()

	mcpTool := &mcp.Tool{
		Name:        def.Name,
		Description: def.Description,
		InputSchema: parameterSchemaToJSON(def.Parameters),
	}

	// Capture tool in closure.
	t := tool
	server.AddTool(mcpTool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]any
		if req.Params.Arguments != nil {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: "invalid arguments: " + err.Error()}},
					IsError: true,
				}, nil
			}
		}
		if args == nil {
			args = make(map[string]any)
		}

		result, err := t.Execute(ctx, args)
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
				IsError: true,
			}, nil
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: result}},
		}, nil
	})
}

// parameterSchemaToJSON converts saige's ParameterSchema to a JSON Schema map
// suitable for MCP's InputSchema field.
func parameterSchemaToJSON(ps agenttypes.ParameterSchema) map[string]any {
	schema := map[string]any{
		"type": ps.Type,
	}
	if len(ps.Required) > 0 {
		schema["required"] = ps.Required
	}
	if len(ps.Properties) > 0 {
		props := make(map[string]any, len(ps.Properties))
		for name, prop := range ps.Properties {
			props[name] = propertyDefToJSON(prop)
		}
		schema["properties"] = props
	}
	return schema
}

func propertyDefToJSON(pd agenttypes.PropertyDef) map[string]any {
	prop := map[string]any{
		"type": pd.Type,
	}
	if pd.Description != "" {
		prop["description"] = pd.Description
	}
	if len(pd.Enum) > 0 {
		prop["enum"] = pd.Enum
	}
	if pd.Items != nil {
		prop["items"] = propertyDefToJSON(*pd.Items)
	}
	if len(pd.Properties) > 0 {
		nested := make(map[string]any, len(pd.Properties))
		for name, p := range pd.Properties {
			nested[name] = propertyDefToJSON(p)
		}
		prop["properties"] = nested
	}
	if len(pd.Required) > 0 {
		prop["required"] = pd.Required
	}
	if pd.Default != nil {
		prop["default"] = pd.Default
	}
	return prop
}
