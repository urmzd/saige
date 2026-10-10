package anthropic

import (
	"context"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/types"
)

var _ catalog.ModelLister = (*Adapter)(nil)

// WithToolChoice constrains whether and which tool the model calls: auto,
// none, required (Anthropic "any") or a named tool. The model must declare
// CapToolChoice. A manual thinking budget accepts only auto and none, and a
// model declaring RejectsForcedToolChoice refuses required and named, so
// those combinations fail locally. Adaptive thinking accepts forcing.
//
// The choice applies to requests that offer tools, local or server. A request
// without any tools sends no choice. A named choice must name a local tool.
// A schema on a model that takes it as a hidden tool forces that tool, so
// such a request rejects any choice other than auto.
func WithToolChoice(c types.ToolChoice) Option {
	return func(a *Adapter) { a.toolChoice = &c }
}

// WithServerTools enables tools Anthropic runs itself: web search and code
// execution. Their calls and results stream back as ServerToolCallPart and
// ServerToolResultPart parts; no local gate sees them. A later turn replays
// them natively while the tool is still offered. Kinds the model does not
// declare fail locally, and remote MCP is rejected because it needs the MCP
// connector, which this adapter does not send.
func WithServerTools(tools ...types.ServerTool) Option {
	return func(a *Adapter) { a.serverTools = append(a.serverTools, tools...) }
}

// validateTools checks the server tools and whether a forced tool choice can
// be sent. Validate calls it after the generic option checks.
func (a *Adapter) validateTools(caps types.ModelCapabilities) error {
	if err := types.ValidateServerTools(caps, a.serverTools); err != nil {
		return caps.OptionError("server_tools", err.Error())
	}
	for _, st := range a.serverTools {
		if st.Kind == types.ServerToolRemoteMCP {
			return caps.OptionError("server_tools", "remote MCP needs the MCP connector, which this adapter does not send")
		}
	}
	if a.toolChoice != nil && a.toolChoice.Forced() {
		if why := a.forcedToolBlocked(caps); why != "" {
			return caps.OptionError("tool_choice", why)
		}
	}
	return nil
}

// checkToolChoice validates the configured choice against the offered tools.
// A request that offers neither local nor server tools is exempt: the choice
// is not sent. With only server tools, auto, none and required apply to them,
// and a named choice fails because it can name only a local tool.
func (a *Adapter) checkToolChoice(tools []types.ToolDef) error {
	if a.toolChoice == nil || (len(tools) == 0 && len(a.serverTools) == 0) {
		return nil
	}
	if len(tools) == 0 && a.toolChoice.Mode != types.ToolChoiceNamed {
		// Nil skips the local tool-list checks; the server tools satisfy them.
		return a.Capabilities().ValidateToolChoice(a.toolChoice, nil)
	}
	if tools == nil {
		tools = []types.ToolDef{}
	}
	return a.Capabilities().ValidateToolChoice(a.toolChoice, tools)
}

// checkSchemaToolChoice rejects a choice that the forced structured-output
// tool would override. Only auto, or no choice, is compatible.
func (a *Adapter) checkSchemaToolChoice() error {
	if a.toolChoice == nil || a.toolChoice.Mode == "" || a.toolChoice.Mode == types.ToolChoiceAuto {
		return nil
	}
	return a.Capabilities().OptionError("tool_choice", "conflicts with forced-tool structured output")
}

// applyToolChoice encodes the configured tool choice and the parallel-call
// setting. A request that already forces a tool keeps it, and a request that
// offers no tools, local or server, carries only the parallel-call setting.
func (a *Adapter) applyToolChoice(p *anthropic.MessageNewParams, offered bool) {
	if p.ToolChoice.OfTool != nil {
		return
	}
	var disable param.Opt[bool]
	if a.parallelTools != nil {
		disable = anthropic.Bool(!*a.parallelTools)
	}
	mode := types.ToolChoiceAuto
	if a.toolChoice != nil && offered {
		mode = a.toolChoice.Mode
	}
	switch mode {
	case types.ToolChoiceNone:
		p.ToolChoice = anthropic.ToolChoiceUnionParam{OfNone: &anthropic.ToolChoiceNoneParam{}}
	case types.ToolChoiceRequired:
		p.ToolChoice = anthropic.ToolChoiceUnionParam{OfAny: &anthropic.ToolChoiceAnyParam{DisableParallelToolUse: disable}}
	case types.ToolChoiceNamed:
		p.ToolChoice = anthropic.ToolChoiceUnionParam{OfTool: &anthropic.ToolChoiceToolParam{Name: a.toolChoice.Name, DisableParallelToolUse: disable}}
	default:
		if a.parallelTools != nil || (a.toolChoice != nil && offered) {
			p.ToolChoice = anthropic.ToolChoiceUnionParam{OfAuto: &anthropic.ToolChoiceAutoParam{DisableParallelToolUse: disable}}
		}
	}
}

// serverToolParams converts the configured server tools to their native
// declarations. Validate has already rejected kinds this adapter cannot send.
func (a *Adapter) serverToolParams() []anthropic.ToolUnionParam {
	var out []anthropic.ToolUnionParam
	for _, st := range a.serverTools {
		switch st.Kind {
		case types.ServerToolWebSearch:
			ws := &anthropic.WebSearchTool20250305Param{
				AllowedDomains: st.AllowedDomains,
				BlockedDomains: st.BlockedDomains,
			}
			if st.MaxUses > 0 {
				ws.MaxUses = anthropic.Int(int64(st.MaxUses))
			}
			if loc, ok := userLocation(st.UserLocation); ok {
				ws.UserLocation = loc
			}
			out = append(out, anthropic.ToolUnionParam{OfWebSearchTool20250305: ws})
		case types.ServerToolCodeExecution:
			out = append(out, anthropic.ToolUnionParam{OfCodeExecutionTool20250825: &anthropic.CodeExecutionTool20250825Param{}})
		}
	}
	return out
}

// userLocation reads "US", "London, GB", "Austin, Texas, US" or "Paris" into
// an approximate location. A trailing two-letter part is the country code; of
// the remaining parts the first is the city and the second the region.
func userLocation(s string) (anthropic.UserLocationParam, bool) {
	var parts []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return anthropic.UserLocationParam{}, false
	}
	var loc anthropic.UserLocationParam
	last := parts[len(parts)-1]
	if len(last) == 2 {
		loc.Country = anthropic.String(strings.ToUpper(last))
		parts = parts[:len(parts)-1]
	}
	if len(parts) > 0 {
		loc.City = anthropic.String(parts[0])
	}
	if len(parts) > 1 {
		loc.Region = anthropic.String(parts[1])
	}
	return loc, true
}

// serverToolKind maps a native server tool name to its provider-neutral kind.
// Code execution reports its bash and editor sub-tools under its own kind.
func serverToolKind(name string) types.ServerToolKind {
	switch {
	case name == "web_search":
		return types.ServerToolWebSearch
	case strings.Contains(name, "code_execution"):
		return types.ServerToolCodeExecution
	default:
		return types.ServerToolKind(name)
	}
}

// ListModels implements catalog.ModelLister with the Models API, following
// pagination to the end.
func (a *Adapter) ListModels(ctx context.Context) ([]catalog.RemoteModel, error) {
	pager := a.client.Models.ListAutoPaging(ctx, anthropic.ModelListParams{})
	var out []catalog.RemoteModel
	for pager.Next() {
		m := pager.Current()
		out = append(out, catalog.RemoteModel{
			ID:              m.ID,
			DisplayName:     m.DisplayName,
			Created:         m.CreatedAt,
			ContextWindow:   int(m.MaxInputTokens),
			MaxOutputTokens: int(m.MaxTokens),
		})
	}
	if err := pager.Err(); err != nil {
		return nil, classifyAnthropicError(string(a.model), err, true)
	}
	return out, nil
}
