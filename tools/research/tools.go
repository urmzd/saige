// Package research provides agent tools for web search, file operations,
// and knowledge graph CRUD.
package research

import (
	agenttypes "github.com/urmzd/saige/agent/types"
	kgtypes "github.com/urmzd/saige/knowledge/types"
	"github.com/urmzd/saige/rag/source/searxng"
)

// NewTools returns research tools for use with an agent.
// Pass nil for searcher to omit the web_search tool.
// Pass nil for graph to omit knowledge graph tools.
// Pass "" for root to use the current working directory for file tools.
func NewTools(s *searxng.Client, graph kgtypes.Graph, root string) []agenttypes.Tool {
	if root == "" {
		root = "."
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
		tools = append(tools,
			NewSearchKnowledgeTool(graph),
			NewStoreKnowledgeTool(graph),
			NewGetGraphTool(graph),
		)
	}

	return tools
}
