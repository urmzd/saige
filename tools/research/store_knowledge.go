package research

import (
	"context"
	"fmt"

	"github.com/urmzd/saige/agent/types"
	kgtypes "github.com/urmzd/saige/knowledge/types"
)

// StoreKnowledgeTool implements types.Tool for storing knowledge.
type StoreKnowledgeTool struct {
	graph   kgtypes.Graph
	groupID string
}

func NewStoreKnowledgeTool(graph kgtypes.Graph) *StoreKnowledgeTool {
	return &StoreKnowledgeTool{graph: graph}
}

func (t *StoreKnowledgeTool) WithGroupID(id string) *StoreKnowledgeTool {
	return &StoreKnowledgeTool{graph: t.graph, groupID: id}
}

func (t *StoreKnowledgeTool) Definition() types.ToolDef {
	return types.ToolDef{
		Name:        "store_knowledge",
		Description: "Store information into the knowledge graph by extracting entities and relationships from text. Use this to persist important findings.",
		Parameters: types.ParameterSchema{
			Type:     "object",
			Required: []string{"text", "source"},
			Properties: map[string]types.PropertyDef{
				"text":   {Type: "string", Description: "The text content to extract knowledge from"},
				"source": {Type: "string", Description: "Description of the source of this information"},
			},
		},
	}
}

func (t *StoreKnowledgeTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	text, _ := args["text"].(string)
	if text == "" {
		return "", fmt.Errorf("store_knowledge: text is required")
	}
	source, _ := args["source"].(string)
	if source == "" {
		source = "chat"
	}

	input := &kgtypes.EpisodeInput{
		Name:    source,
		Body:    text,
		Source:  source,
		GroupID: t.groupID,
	}

	resp, err := t.graph.IngestEpisode(ctx, input)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("Stored %d entities and %d relations.", len(resp.EntityNodes), len(resp.EpisodicEdges)), nil
}
