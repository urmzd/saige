package research

import (
	"context"
	"fmt"

	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/knowledge/graph"
	kgtypes "github.com/urmzd/saige/knowledge/types"
)

// GetGraphTool implements types.Tool for knowledge graph visualization.
type GetGraphTool struct {
	graph kgtypes.Graph
}

func NewGetGraphTool(g kgtypes.Graph) *GetGraphTool {
	return &GetGraphTool{graph: g}
}

func (t *GetGraphTool) Definition() types.ToolDef {
	return types.ToolDef{
		Name:        "get_knowledge_graph",
		Description: "Get the knowledge graph structure showing all entities and their relationships. Returns a readable summary of how stored knowledge is connected.",
		Parameters: types.ParameterSchema{
			Type: "object",
			Properties: map[string]types.PropertyDef{
				"limit": {Type: "number", Description: "Max edges to return (default 100)"},
			},
		},
	}
}

func (t *GetGraphTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	limit := int64(100)
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int64(l)
	}

	data, err := t.graph.GetGraph(ctx, limit)
	if err != nil {
		return "", fmt.Errorf("get_knowledge_graph: %w", err)
	}

	text := graph.ToText(data)
	if text == "" {
		return "Knowledge graph is empty.", nil
	}
	return text, nil
}
