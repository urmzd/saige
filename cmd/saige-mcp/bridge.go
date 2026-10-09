package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	saigemcp "github.com/urmzd/saige/agent/mcp"
	agenttypes "github.com/urmzd/saige/agent/types"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// approvalMode decides what the bridge does with a tool that carries a
// marker. Inside saige the agent loop enforces markers; over
// MCP there is no agent loop, so the bridge must.
type approvalMode string

const (
	// approvalElicit asks the user through MCP elicitation and runs the tool
	// only on an explicit yes. A client without elicitation gets a refusal
	// that names the alternatives.
	approvalElicit approvalMode = "elicit"
	// approvalHost runs marked tools directly and relies on the MCP client's
	// own per-tool permission prompt. The tools are published with
	// destructiveHint so the client knows to ask.
	approvalHost approvalMode = "host"
	// approvalDeny refuses every marked tool.
	approvalDeny approvalMode = "deny"
)

func parseApprovalMode(s string) (approvalMode, error) {
	switch m := approvalMode(strings.ToLower(strings.TrimSpace(s))); m {
	case approvalElicit, approvalHost, approvalDeny:
		return m, nil
	}
	return "", fmt.Errorf("unknown --approval %q: use elicit, host or deny", s)
}

// bridge publishes saige tools on an MCP server.
type bridge struct {
	approval approvalMode
}

// approvalMarker returns the marker that gates tool, looking through nested
// marker wrappers. Inside saige the agent loop pauses on every marker kind,
// so the bridge treats every marker as needing a human decision: a kind it
// does not enforce itself, such as audit or rate_limit, would otherwise run
// with nothing applied. A human_approval or mutating marker is preferred
// because its message describes the approval.
func approvalMarker(tool agenttypes.Tool) (agenttypes.Marker, bool) {
	var first agenttypes.Marker
	found := false
	for {
		marked, ok := tool.(*agenttypes.MarkedTool)
		if !ok {
			return first, found
		}
		for _, m := range marked.Markers {
			if m.Kind == "human_approval" {
				return m, true
			}
			if mutating, _ := m.Meta["mutating"].(bool); mutating {
				return m, true
			}
			if !found {
				first, found = m, true
			}
		}
		tool = marked.Inner
	}
}

// annotationsFor derives MCP tool annotations. A tool that needs approval is
// always published as destructive, whatever its declared capability, so the
// client's own permission prompt treats it as such.
func annotationsFor(tool agenttypes.Tool) *mcp.ToolAnnotations {
	if _, ok := approvalMarker(tool); ok {
		t := true
		return &mcp.ToolAnnotations{DestructiveHint: &t}
	}
	return saigemcp.AnnotationsForCapability(tool.Definition().Capability)
}

// register bridges a saige Tool into an MCP server.
func (b bridge) register(server *mcp.Server, tool agenttypes.Tool) {
	def := tool.Definition()

	mcpTool := &mcp.Tool{
		Name:        def.Name,
		Description: def.Description,
		InputSchema: parameterSchemaToJSON(def.Parameters),
		Annotations: annotationsFor(tool),
	}

	marker, needsApproval := approvalMarker(tool)
	t := tool
	server.AddTool(mcpTool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]any
		if req.Params.Arguments != nil {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return errorResult("invalid arguments: " + err.Error()), nil
			}
		}
		if args == nil {
			args = make(map[string]any)
		}

		if needsApproval {
			if refusal := b.approve(ctx, req.Session, def.Name, marker, args); refusal != "" {
				return errorResult(refusal), nil
			}
		}
		return execute(ctx, t, args), nil
	})
}

// approve returns "" when the call may run, or the refusal to send back.
func (b bridge) approve(ctx context.Context, session *mcp.ServerSession, name string, marker agenttypes.Marker, args map[string]any) string {
	switch b.approval {
	case approvalHost:
		return ""
	case approvalDeny:
		return fmt.Sprintf("%s requires human approval, and this server runs with --approval=deny", name)
	}

	if session == nil || !supportsElicitation(session) {
		return fmt.Sprintf("%s requires human approval, but this MCP client does not support elicitation. "+
			"Restart saige-mcp with --approval=host to rely on the client's own permission prompt, or with --read-only to drop mutating tools", name)
	}
	message := marker.Message
	if message == "" {
		message = name + " needs your approval"
		if marker.Kind != "" && marker.Kind != "human_approval" {
			message = name + " is marked " + marker.Kind + " and needs your approval"
		}
	}
	res, err := session.Elicit(ctx, &mcp.ElicitParams{
		Message: message + "\n\nArguments: " + summarizeArgs(args),
		RequestedSchema: parameterSchemaToJSON(agenttypes.ParameterSchema{
			Type:     agenttypes.SchemaObject,
			Required: []string{"approve"},
			Properties: map[string]agenttypes.PropertyDef{
				"approve": {Type: agenttypes.SchemaBoolean, Description: "Run " + name + " with these arguments"},
			},
		}),
	})
	if err != nil {
		return fmt.Sprintf("%s requires human approval, and the approval request failed: %v", name, err)
	}
	if res.Action != "accept" {
		return fmt.Sprintf("%s was not approved (%s)", name, res.Action)
	}
	if ok, _ := res.Content["approve"].(bool); !ok {
		return fmt.Sprintf("%s was not approved", name)
	}
	return ""
}

func supportsElicitation(session *mcp.ServerSession) bool {
	p := session.InitializeParams()
	return p != nil && p.Capabilities != nil && p.Capabilities.Elicitation != nil
}

// summarizeArgs renders arguments for an approval prompt, cutting long values
// so the prompt stays readable.
func summarizeArgs(args map[string]any) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		raw, _ := json.Marshal(args[k])
		v := string(raw)
		if len(v) > 200 {
			v = v[:200] + "..."
		}
		parts = append(parts, k+"="+v)
	}
	if len(parts) == 0 {
		return "(none)"
	}
	return strings.Join(parts, ", ")
}

// execute runs the tool and maps its result. A RichTool's blocks are carried
// over, so images and structured output survive the bridge.
func execute(ctx context.Context, tool agenttypes.Tool, args map[string]any) *mcp.CallToolResult {
	rich, ok := tool.(agenttypes.RichTool)
	if !ok {
		text, err := tool.Execute(ctx, args)
		if err != nil {
			return errorResult(err.Error())
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
	}

	res, err := rich.ExecuteRich(ctx, args)
	if err != nil {
		return errorResult(err.Error())
	}
	out := &mcp.CallToolResult{IsError: res.IsError}
	hasText := false
	for _, b := range res.Blocks {
		switch b.Kind {
		case agenttypes.ToolResultBlockText:
			hasText = true
			out.Content = append(out.Content, &mcp.TextContent{Text: b.Text})
		case agenttypes.ToolResultBlockImage:
			out.Content = append(out.Content, &mcp.ImageContent{MIMEType: string(b.MediaType), Data: b.Data})
		case agenttypes.ToolResultBlockFile:
			if strings.HasPrefix(string(b.MediaType), "audio/") {
				out.Content = append(out.Content, &mcp.AudioContent{MIMEType: string(b.MediaType), Data: b.Data})
			} else if b.URI != "" || len(b.Data) > 0 {
				out.Content = append(out.Content, &mcp.EmbeddedResource{Resource: &mcp.ResourceContents{
					URI: b.URI, MIMEType: string(b.MediaType), Blob: b.Data,
				}})
			}
		case agenttypes.ToolResultBlockJSON:
			// MCP structured content must be a JSON object.
			var obj map[string]any
			if json.Unmarshal(b.JSON, &obj) == nil && obj != nil {
				out.StructuredContent = obj
			}
		}
	}
	// The text projection is mandatory; send it when no block carried text.
	if !hasText && (res.Text != "" || len(out.Content) == 0) {
		out.Content = append([]mcp.Content{&mcp.TextContent{Text: res.Text}}, out.Content...)
	}
	return out
}

func errorResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, IsError: true}
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
	return pd.JSONSchema()
}
