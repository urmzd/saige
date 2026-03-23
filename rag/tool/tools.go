// Package tool provides agent Tool implementations for RAG Pipeline operations.
package tool

import (
	"context"
	"encoding/json"
	"fmt"

	agenttypes "github.com/urmzd/saige/agent/types"
	ragtypes "github.com/urmzd/saige/rag/types"
)

// --- Parameter types for SchemaFrom ---

type searchParams struct {
	Query        string   `json:"query" description:"Search query text"`
	Limit        int      `json:"limit,omitempty" description:"Maximum number of results"`
	ContentTypes []string `json:"content_types,omitempty" description:"Filter by content types"`
	MinScore     float64  `json:"min_score,omitempty" description:"Minimum relevance score threshold"`
}

type lookupParams struct {
	VariantUUID string `json:"variant_uuid" description:"UUID of the variant to look up"`
}

type updateParams struct {
	DocumentUUID string `json:"document_uuid" description:"UUID of the document to update"`
	SourceURI    string `json:"source_uri" description:"Source URI of the new content"`
	MIMEType     string `json:"mime_type" description:"MIME type of the new content"`
	Data         string `json:"data" description:"Base64-encoded or plain text content data"`
}

type deleteParams struct {
	DocumentUUID string `json:"document_uuid" description:"UUID of the document to delete"`
}

type reconstructParams struct {
	DocumentUUID string `json:"document_uuid" description:"UUID of the document to reconstruct"`
}

// --- SearchTool ---

// SearchTool searches the pipeline and returns provenance-only hits.
type SearchTool struct {
	pipeline ragtypes.Pipeline
}

func (t *SearchTool) Definition() agenttypes.ToolDef {
	return agenttypes.ToolDef{
		Name:        "rag_search",
		Description: "Search the knowledge base. Returns scored hits with provenance metadata (no full content — use rag_lookup to dereference).",
		Parameters:  agenttypes.SchemaFrom[searchParams](),
	}
}

func (t *SearchTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	query, _ := args["query"].(string)
	if query == "" {
		return "", fmt.Errorf("query is required")
	}

	var opts []ragtypes.SearchOption
	if limit, ok := toInt(args["limit"]); ok && limit > 0 {
		opts = append(opts, ragtypes.WithLimit(limit))
	}
	if minScore, ok := args["min_score"].(float64); ok && minScore > 0 {
		opts = append(opts, ragtypes.WithMinScore(minScore))
	}
	if ctRaw, ok := args["content_types"].([]any); ok {
		var ctypes []ragtypes.ContentType
		for _, ct := range ctRaw {
			if s, ok := ct.(string); ok {
				ctypes = append(ctypes, ragtypes.ContentType(s))
			}
		}
		if len(ctypes) > 0 {
			opts = append(opts, ragtypes.WithContentTypes(ctypes...))
		}
	}

	result, err := t.pipeline.Search(ctx, query, opts...)
	if err != nil {
		return "", err
	}

	// Return provenance-only hits (no full content to keep agent context lean).
	type provenanceHit struct {
		VariantUUID string             `json:"variant_uuid"`
		Score       float64            `json:"score"`
		ContentType ragtypes.ContentType `json:"content_type"`
		Provenance  ragtypes.Provenance  `json:"provenance"`
	}
	hits := make([]provenanceHit, len(result.Hits))
	for i, h := range result.Hits {
		hits[i] = provenanceHit{
			VariantUUID: h.Variant.UUID,
			Score:       h.Score,
			ContentType: h.Variant.ContentType,
			Provenance:  h.Provenance,
		}
	}

	data, err := json.Marshal(hits)
	if err != nil {
		return "", fmt.Errorf("marshal results: %w", err)
	}
	return string(data), nil
}

// --- LookupTool ---

// LookupTool retrieves full content for a specific variant by UUID.
type LookupTool struct {
	pipeline ragtypes.Pipeline
}

func (t *LookupTool) Definition() agenttypes.ToolDef {
	return agenttypes.ToolDef{
		Name:        "rag_lookup",
		Description: "Look up a specific content variant by UUID. Returns full content with provenance.",
		Parameters:  agenttypes.SchemaFrom[lookupParams](),
	}
}

func (t *LookupTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	uuid, _ := args["variant_uuid"].(string)
	if uuid == "" {
		return "", fmt.Errorf("variant_uuid is required")
	}

	hit, err := t.pipeline.Lookup(ctx, uuid)
	if err != nil {
		return "", err
	}

	data, err := json.Marshal(hit)
	if err != nil {
		return "", fmt.Errorf("marshal hit: %w", err)
	}
	return string(data), nil
}

// --- UpdateTool ---

// UpdateTool re-ingests a document with new content.
type UpdateTool struct {
	pipeline ragtypes.Pipeline
}

func (t *UpdateTool) Definition() agenttypes.ToolDef {
	return agenttypes.ToolDef{
		Name:        "rag_update",
		Description: "Update a document by re-ingesting with new content. Deletes the old version first.",
		Parameters:  agenttypes.SchemaFrom[updateParams](),
	}
}

func (t *UpdateTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	docUUID, _ := args["document_uuid"].(string)
	if docUUID == "" {
		return "", fmt.Errorf("document_uuid is required")
	}
	sourceURI, _ := args["source_uri"].(string)
	mimeType, _ := args["mime_type"].(string)
	rawData, _ := args["data"].(string)

	result, err := t.pipeline.Update(ctx, docUUID, &ragtypes.RawDocument{
		SourceURI: sourceURI,
		MIMEType:  mimeType,
		Data:      []byte(rawData),
	})
	if err != nil {
		return "", err
	}

	data, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("marshal result: %w", err)
	}
	return string(data), nil
}

// --- DeleteTool ---

// DeleteTool deletes a document from the pipeline.
type DeleteTool struct {
	pipeline ragtypes.Pipeline
}

func (t *DeleteTool) Definition() agenttypes.ToolDef {
	return agenttypes.ToolDef{
		Name:        "rag_delete",
		Description: "Delete a document from the knowledge base by UUID.",
		Parameters:  agenttypes.SchemaFrom[deleteParams](),
	}
}

func (t *DeleteTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	docUUID, _ := args["document_uuid"].(string)
	if docUUID == "" {
		return "", fmt.Errorf("document_uuid is required")
	}

	if err := t.pipeline.Delete(ctx, docUUID); err != nil {
		return "", err
	}
	return `{"status":"deleted"}`, nil
}

// --- ReconstructTool ---

// ReconstructTool reconstructs a full document structure.
type ReconstructTool struct {
	pipeline ragtypes.Pipeline
}

func (t *ReconstructTool) Definition() agenttypes.ToolDef {
	return agenttypes.ToolDef{
		Name:        "rag_reconstruct",
		Description: "Reconstruct the full document structure including all sections and variants.",
		Parameters:  agenttypes.SchemaFrom[reconstructParams](),
	}
}

func (t *ReconstructTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	docUUID, _ := args["document_uuid"].(string)
	if docUUID == "" {
		return "", fmt.Errorf("document_uuid is required")
	}

	doc, err := t.pipeline.Reconstruct(ctx, docUUID)
	if err != nil {
		return "", err
	}

	data, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("marshal document: %w", err)
	}
	return string(data), nil
}

// --- NewTools ---

// NewTools returns all 5 rag tools for use with agent.
func NewTools(pipeline ragtypes.Pipeline) []agenttypes.Tool {
	return []agenttypes.Tool{
		&SearchTool{pipeline: pipeline},
		&LookupTool{pipeline: pipeline},
		&UpdateTool{pipeline: pipeline},
		&DeleteTool{pipeline: pipeline},
		&ReconstructTool{pipeline: pipeline},
	}
}

// toInt converts a JSON number (float64) to int.
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
