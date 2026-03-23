// Package tool provides agent Tool implementations for Knowledge Graph operations.
package tool

import (
	"context"
	"encoding/json"
	"fmt"

	agenttypes "github.com/urmzd/saige/agent/types"
	kgtypes "github.com/urmzd/saige/knowledge/types"
)

// --- Parameter types ---

type searchParams struct {
	Query string `json:"query" description:"Search query text"`
	Limit int    `json:"limit,omitempty" description:"Maximum number of results"`
}

type ingestParams struct {
	Name   string `json:"name" description:"Episode name/title"`
	Body   string `json:"body" description:"Episode text content to extract entities and relations from"`
	Source string `json:"source,omitempty" description:"Source description for provenance"`
}

// --- SearchTool ---

// SearchTool searches the knowledge graph for facts.
type SearchTool struct {
	graph kgtypes.Graph
}

func (t *SearchTool) Definition() agenttypes.ToolDef {
	return agenttypes.ToolDef{
		Name:        "kg_search",
		Description: "Search the knowledge graph for facts matching the query. Returns scored facts with entity names, relation types, and confidence.",
		Parameters:  agenttypes.SchemaFrom[searchParams](),
	}
}

func (t *SearchTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	query, _ := args["query"].(string)
	if query == "" {
		return "", fmt.Errorf("query is required")
	}

	var opts []kgtypes.SearchOption
	if limit, ok := toInt(args["limit"]); ok && limit > 0 {
		opts = append(opts, kgtypes.WithLimit(limit))
	}

	result, err := t.graph.SearchFacts(ctx, query, opts...)
	if err != nil {
		return "", fmt.Errorf("kg search: %w", err)
	}

	data, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}
	return string(data), nil
}

// --- IngestTool ---

// IngestTool ingests text into the knowledge graph, extracting entities and relations.
type IngestTool struct {
	graph kgtypes.Graph
}

func (t *IngestTool) Definition() agenttypes.ToolDef {
	return agenttypes.ToolDef{
		Name:        "kg_ingest",
		Description: "Ingest text into the knowledge graph. Extracts entities and relations from the provided text.",
		Parameters:  agenttypes.SchemaFrom[ingestParams](),
	}
}

func (t *IngestTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	name, _ := args["name"].(string)
	body, _ := args["body"].(string)
	if name == "" || body == "" {
		return "", fmt.Errorf("name and body are required")
	}

	source, _ := args["source"].(string)

	result, err := t.graph.IngestEpisode(ctx, &kgtypes.EpisodeInput{
		Name:   name,
		Body:   body,
		Source: source,
	})
	if err != nil {
		return "", fmt.Errorf("kg ingest: %w", err)
	}

	data, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}
	return string(data), nil
}

// --- NewTools ---

// NewTools returns KG tools for use with an agent.
func NewTools(graph kgtypes.Graph) []agenttypes.Tool {
	return []agenttypes.Tool{
		&SearchTool{graph: graph},
		&IngestTool{graph: graph},
	}
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	default:
		return 0, false
	}
}
