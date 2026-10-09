package research

import (
	"context"
	"errors"
	"fmt"

	"github.com/urmzd/saige/agent/types"
	kgtypes "github.com/urmzd/saige/rag/knowledge/types"
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
		Capability:  types.ToolCapabilityWrite,
		Description: "Store information into the knowledge graph by extracting entities and relationships from text. Use this to persist important findings.",
		Parameters: types.ParameterSchema{
			Type:     types.SchemaObject,
			Required: []string{"text", "source"},
			Properties: map[string]types.PropertyDef{
				"text":   {Type: types.SchemaString, Description: "The text content to extract knowledge from"},
				"source": {Type: types.SchemaString, Description: "Description of the source of this information"},
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
		// A partial episode is stored with some facts missing: report what
		// was stored, with the reason the rest was not.
		if !errors.Is(err, kgtypes.ErrPartialEpisode) || resp == nil {
			return "", err
		}
		return fmt.Sprintf("Stored %d entities and %d relations. Warning: %v", len(resp.EntityNodes), len(resp.EpisodicEdges), err), nil
	}

	return fmt.Sprintf("Stored %d entities and %d relations.", len(resp.EntityNodes), len(resp.EpisodicEdges)), nil
}
