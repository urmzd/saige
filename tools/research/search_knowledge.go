package research

import (
	"context"
	"fmt"
	"strings"

	"github.com/urmzd/saige/agent/types"
	kgtypes "github.com/urmzd/saige/knowledge/types"
)

// SearchKnowledgeTool implements types.Tool for knowledge graph search.
type SearchKnowledgeTool struct {
	graph   kgtypes.Graph
	groupID string
}

func NewSearchKnowledgeTool(graph kgtypes.Graph) *SearchKnowledgeTool {
	return &SearchKnowledgeTool{graph: graph}
}

func (t *SearchKnowledgeTool) WithGroupID(id string) *SearchKnowledgeTool {
	return &SearchKnowledgeTool{graph: t.graph, groupID: id}
}

func (t *SearchKnowledgeTool) Definition() types.ToolDef {
	return types.ToolDef{
		Name:        "search_knowledge",
		Description: "Search the knowledge graph for previously stored facts and entities. Returns up to 10 relevant facts.",
		Parameters: types.ParameterSchema{
			Type:     "object",
			Required: []string{"query"},
			Properties: map[string]types.PropertyDef{
				"query": {Type: "string", Description: "The search query for knowledge retrieval"},
			},
		},
	}
}

func (t *SearchKnowledgeTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	query, _ := args["query"].(string)
	if query == "" {
		return "", fmt.Errorf("search_knowledge: query is required")
	}

	opts := []kgtypes.SearchOption{kgtypes.WithLimit(10)}
	if t.groupID != "" {
		opts = append(opts, kgtypes.WithGroupID(t.groupID))
	}

	resp, err := t.graph.SearchFacts(ctx, query, opts...)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	for _, f := range resp.Facts {
		fmt.Fprintf(&b, "- %s -> %s: %s\n", f.SourceNode.Name, f.TargetNode.Name, f.FactText)
	}

	if b.Len() == 0 {
		return "No relevant knowledge found.", nil
	}
	return b.String(), nil
}
