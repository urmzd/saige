// Package research provides agent tools for web search, file operations,
// and knowledge graph CRUD.
package research

import (
	agenttypes "github.com/urmzd/saige/agent/types"
	kgtypes "github.com/urmzd/saige/knowledge/types"
	"github.com/urmzd/saige/rag/source/searxng"
)

// config controls how NewTools assembles the research tool set.
type config struct {
	readOnly bool
}

// Option configures NewTools.
type Option func(*config)

// ReadOnly omits every mutating tool (store_knowledge) from the returned set,
// exposing only read/search tools. Use this for untrusted or query-only agents.
func ReadOnly() Option {
	return func(c *config) { c.readOnly = true }
}

// NewTools returns research tools for use with an agent.
// Pass nil for searcher to omit the web_search tool.
// Pass nil for graph to omit knowledge graph tools.
// Pass "" for root to use the current working directory for file tools.
//
// By default the mutating store_knowledge tool is wrapped in a "human_approval"
// marker so the agent loop pauses for approval before it persists to the graph.
// Pass ReadOnly() to omit store_knowledge entirely instead.
func NewTools(s *searxng.Client, graph kgtypes.Graph, root string, opts ...Option) []agenttypes.Tool {
	if root == "" {
		root = "."
	}

	cfg := config{}
	for _, o := range opts {
		o(&cfg)
	}

	var tools []agenttypes.Tool

	if s != nil {
		tools = append(tools, NewWebSearchTool(s, graph))
	}

	tools = append(tools,
		NewFileSearchTool(root),
		NewReadFileTool(root),
	)

	if graph != nil {
		tools = append(tools, NewSearchKnowledgeTool(graph))
		if !cfg.readOnly {
			tools = append(tools, agenttypes.WithMarkers(
				NewStoreKnowledgeTool(graph),
				agenttypes.Marker{
					Kind:    "human_approval",
					Message: "Mutating knowledge-graph operation requires human approval: store_knowledge",
					Meta:    map[string]any{"tool": "store_knowledge", "mutating": true},
				},
			))
		}
		tools = append(tools, NewGetGraphTool(graph))
	}

	return tools
}
