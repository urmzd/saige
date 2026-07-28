package types

import "fmt"

// ServerToolKind names a tool the *provider* executes, not this process.
//
// The distinction matters more than it first appears. A local tool runs here:
// this SDK sees the arguments, can gate the call, and owns the result. A server
// tool runs inside the provider: the model calls it and answers from the
// result in the same turn, so there is no local execution to gate, no
// ToolExecStartDelta, and the only trace is whatever citation metadata comes
// back. Choosing one over the other is a trust and observability decision, not
// just a convenience one.
type ServerToolKind string

const (
	// ServerToolWebSearch: the provider runs a web search and grounds its
	// answer in the results (Anthropic web_search, Gemini google_search
	// grounding, OpenAI web_search).
	ServerToolWebSearch ServerToolKind = "web_search"
	// ServerToolCodeExecution: the provider runs generated code in its own
	// sandbox and uses the output.
	ServerToolCodeExecution ServerToolKind = "code_execution"
	// ServerToolRemoteMCP: the provider connects to a remote MCP server itself
	// and calls its tools. See RemoteMCPServer for why this is not the same as
	// this SDK connecting to that server (agent/mcp).
	ServerToolRemoteMCP ServerToolKind = "remote_mcp"
)

// ServerTool is a provider-neutral request for a server-executed tool. Adapters
// translate it into their native shape and ignore fields their provider does
// not express; ModelCapabilities says up front which kinds a model supports, so
// a caller can find out before the request rather than after.
type ServerTool struct {
	Kind ServerToolKind

	// MaxUses caps how many times the provider may invoke the tool in one turn.
	// 0 leaves the provider default. Worth setting on web search: an uncapped
	// search loop is billed per search.
	MaxUses int

	// AllowedDomains and BlockedDomains constrain web search. They are mutually
	// exclusive on some providers, so setting both is rejected by Validate.
	AllowedDomains []string
	BlockedDomains []string

	// UserLocation biases web search results, e.g. "US" or "London, UK".
	UserLocation string

	// MCPServer is required when Kind is ServerToolRemoteMCP.
	MCPServer *RemoteMCPServer
}

// RemoteMCPServer describes an MCP server the *provider* connects to on the
// caller's behalf.
//
// This is the sharp edge in "remote versus local" MCP. When the provider holds
// the connection:
//   - the server's tool calls never reach this process, so a local ToolGate
//     cannot see or block them;
//   - the authorization token is sent to the provider, which then presents it
//     to the third party;
//   - there is no local process to supervise, and no way to inject or rewrite
//     arguments.
//
// When this SDK holds the connection instead (agent/mcp), every call is a
// normal local tool: gateable, loggable, and durable through the StepRunner.
// RequireApproval is the only gate available on the provider-side path, and it
// is enforced by the provider, not here.
type RemoteMCPServer struct {
	// Name identifies the server in tool names and logs.
	Name string
	// URL is the server endpoint.
	URL string
	// AuthorizationToken is forwarded to the provider, which presents it to the
	// server. Prefer the local path (agent/mcp) when a token must not leave
	// this process.
	AuthorizationToken string
	// AllowedTools restricts which of the server's tools the model may call.
	// Empty means every tool the server advertises, which on a third-party
	// server is a larger surface than most callers intend.
	AllowedTools []string
	// RequireApproval asks the provider to pause for approval before invoking
	// the server's tools. Provider-enforced: not every provider honours it, and
	// none of them route the decision through this process.
	RequireApproval bool
}

// Validate reports whether the spec is internally consistent. It catches the
// mistakes that would otherwise surface as an opaque provider 400.
func (s ServerTool) Validate() error {
	switch s.Kind {
	case ServerToolWebSearch:
		if len(s.AllowedDomains) > 0 && len(s.BlockedDomains) > 0 {
			return fmt.Errorf("server tool %s: allowed and blocked domains are mutually exclusive", s.Kind)
		}
	case ServerToolRemoteMCP:
		if s.MCPServer == nil {
			return fmt.Errorf("server tool %s: MCPServer is required", s.Kind)
		}
		if s.MCPServer.URL == "" {
			return fmt.Errorf("server tool %s: MCPServer.URL is required", s.Kind)
		}
		if s.MCPServer.Name == "" {
			return fmt.Errorf("server tool %s: MCPServer.Name is required", s.Kind)
		}
	case ServerToolCodeExecution:
		// No constraints to check.
	default:
		return fmt.Errorf("unknown server tool kind: %q", s.Kind)
	}
	if s.MaxUses < 0 {
		return fmt.Errorf("server tool %s: MaxUses must not be negative", s.Kind)
	}
	return nil
}

// Capability returns the capability a model must declare to accept this server
// tool, so a caller can check support with one lookup.
func (s ServerTool) Capability() Capability {
	switch s.Kind {
	case ServerToolWebSearch:
		return CapWebSearch
	case ServerToolCodeExecution:
		return CapCodeExecution
	case ServerToolRemoteMCP:
		return CapRemoteMCP
	default:
		return CapServerTools
	}
}

// WebSearchTool builds a web-search spec with a use cap.
func WebSearchTool(maxUses int) ServerTool {
	return ServerTool{Kind: ServerToolWebSearch, MaxUses: maxUses}
}

// ValidateServerTools checks a set of specs against a model's declared
// capabilities and returns the first problem found. Calling this before a run
// converts a mid-stream provider error into a startup error, which is the
// difference between a failed deployment and a failed request.
func ValidateServerTools(caps ModelCapabilities, tools []ServerTool) error {
	for _, st := range tools {
		if err := st.Validate(); err != nil {
			return err
		}
		if !caps.Supports(st.Capability()) {
			return fmt.Errorf("model %s/%s does not support server tool %q (missing capability %q)",
				caps.Provider, caps.Model, st.Kind, st.Capability())
		}
	}
	return nil
}
