// Package tool provides agent Tool implementations for Knowledge Graph operations.
package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	agenttypes "github.com/urmzd/saige/agent/types"
	kgtypes "github.com/urmzd/saige/rag/knowledge/types"
)

// MaxSearchLimit caps the number of facts one kg_search call returns,
// whatever limit the model asks for.
const MaxSearchLimit = 50

// --- Parameter types ---

type searchParams struct {
	Query string `json:"query" description:"Search query text"`
	Limit int    `json:"limit,omitempty" description:"Maximum number of results (at most 50)"`
}

type ingestParams struct {
	Name   string `json:"name" description:"Episode name/title"`
	Body   string `json:"body" description:"Episode text content to extract entities and relations from"`
	Source string `json:"source,omitempty" description:"Source description for provenance"`
}

// --- SearchTool ---

// SearchTool searches the knowledge graph for facts.
type SearchTool struct {
	graph   kgtypes.Graph
	groupID string
}

// Definition implements agenttypes.Tool.
func (t *SearchTool) Definition() agenttypes.ToolDef {
	return agenttypes.ToolDef{
		Name:        "kg_search",
		Description: "Search the knowledge graph for facts matching the query. Returns scored facts with entity names, relation types, and confidence.",
		Parameters:  agenttypes.SchemaFrom[searchParams](),
		Capability:  agenttypes.ToolCapabilityRead,
	}
}

// Execute implements agenttypes.Tool.
func (t *SearchTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	query, _ := args["query"].(string)
	if query == "" {
		return "", fmt.Errorf("query is required")
	}

	var opts []kgtypes.SearchOption
	if limit, ok := toInt(args["limit"]); ok && limit > 0 {
		opts = append(opts, kgtypes.WithLimit(min(limit, MaxSearchLimit)))
	}
	if t.groupID != "" {
		opts = append(opts, kgtypes.WithGroupID(t.groupID))
	}

	result, err := t.graph.SearchFacts(ctx, query, opts...)
	// A partial failure still returns usable results; only fail hard when
	// the search produced nothing at all.
	if err != nil && !errors.Is(err, kgtypes.ErrPartialSearch) {
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
	graph   kgtypes.Graph
	groupID string
}

// ingestOutput is the kg_ingest result. Warning reports a partial ingest:
// the episode was stored but some entities or relations were not.
type ingestOutput struct {
	*kgtypes.IngestResult
	Warning string `json:"warning,omitempty"`
}

// Definition implements agenttypes.Tool.
func (t *IngestTool) Definition() agenttypes.ToolDef {
	return agenttypes.ToolDef{
		Name:        "kg_ingest",
		Description: "Ingest text into the knowledge graph. Extracts entities and relations from the provided text.",
		Parameters:  agenttypes.SchemaFrom[ingestParams](),
		Capability:  agenttypes.ToolCapabilityWrite,
	}
}

// Execute implements agenttypes.Tool.
func (t *IngestTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	name, _ := args["name"].(string)
	body, _ := args["body"].(string)
	if name == "" || body == "" {
		return "", fmt.Errorf("name and body are required")
	}

	source, _ := args["source"].(string)

	result, err := t.graph.IngestEpisode(ctx, &kgtypes.EpisodeInput{
		Name:    name,
		Body:    body,
		Source:  source,
		GroupID: t.groupID,
	})
	out := ingestOutput{IngestResult: result}
	if err != nil {
		if !errors.Is(err, kgtypes.ErrPartialEpisode) || result == nil {
			return "", fmt.Errorf("kg ingest: %w", err)
		}
		out.Warning = err.Error()
	}

	data, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}
	return string(data), nil
}

// --- NewTools ---

// config controls how NewTools assembles the tool set.
type config struct {
	readOnly bool
	groupID  string
}

// Option configures NewTools.
type Option func(*config)

// ReadOnly omits every mutating tool (kg_ingest) from the returned set,
// exposing only the safe read tool (kg_search). Use this for untrusted or
// query-only agents.
func ReadOnly() Option {
	return func(c *config) { c.readOnly = true }
}

// WithGroupID binds both tools to one graph group (tenant scope): kg_search
// searches only that group and kg_ingest writes into it. The host sets it;
// the model cannot choose a group. Without it, kg_search searches every
// group and kg_ingest writes to the default group.
func WithGroupID(id string) Option {
	return func(c *config) { c.groupID = id }
}

// mutatingMarker is the human-approval gate attached to mutating KG tools.
func mutatingMarker(tool string) agenttypes.Marker {
	return agenttypes.Marker{
		Kind:    "human_approval",
		Message: "Mutating knowledge-graph operation requires human approval: " + tool,
		Meta:    map[string]any{"tool": tool, "mutating": true},
	}
}

// NewTools returns KG tools for use with an agent.
//
// By default kg_search is returned as-is and the mutating tool (kg_ingest) is
// wrapped in a "human_approval" marker so the agent loop pauses for approval
// before it runs. Pass ReadOnly() to omit kg_ingest entirely instead.
func NewTools(graph kgtypes.Graph, opts ...Option) []agenttypes.Tool {
	cfg := config{}
	for _, o := range opts {
		o(&cfg)
	}

	tools := []agenttypes.Tool{
		&SearchTool{graph: graph, groupID: cfg.groupID},
	}

	if !cfg.readOnly {
		tools = append(tools,
			agenttypes.WithMarkers(&IngestTool{graph: graph, groupID: cfg.groupID}, mutatingMarker("kg_ingest")),
		)
	}

	return tools
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	default:
		return 0, false
	}
}
