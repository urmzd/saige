package extraction

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/urmzd/saige/agent/provider/ollama"
	"github.com/urmzd/saige/rag/knowledge/types"
)

// extractionResponse is the JSON schema returned by the LLM.
type extractionResponse struct {
	Entities  []types.ExtractedEntity   `json:"entities"`
	Relations []types.ExtractedRelation `json:"relations"`
}

// OllamaExtractor uses an Ollama client to extract entities and relations via structured output.
type OllamaExtractor struct {
	client *ollama.Client
}

// NewOllamaExtractor creates a new OllamaExtractor.
func NewOllamaExtractor(client *ollama.Client) *OllamaExtractor {
	return &OllamaExtractor{client: client}
}

// jsonFormat is the Ollama structured output schema for extraction.
var jsonFormat = json.RawMessage(`{
	"type": "object",
	"properties": {
		"entities": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"name": {"type": "string"},
					"type": {"type": "string"},
					"summary": {"type": "string"}
				},
				"required": ["name", "type", "summary"]
			}
		},
		"relations": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"source": {"type": "string"},
					"target": {"type": "string"},
					"type": {"type": "string"},
					"fact": {"type": "string"}
				},
				"required": ["source", "target", "type", "fact"]
			}
		}
	},
	"required": ["entities", "relations"]
}`)

var _ types.OntologyExtractor = (*OllamaExtractor)(nil)

// Extract implements types.Extractor.
func (e *OllamaExtractor) Extract(ctx context.Context, text string) ([]types.ExtractedEntity, []types.ExtractedRelation, error) {
	return e.ExtractWithOntology(ctx, text, nil)
}

// ExtractWithOntology implements types.OntologyExtractor: the prompt lists
// the ontology's entity and relation types so the model uses them.
func (e *OllamaExtractor) ExtractWithOntology(ctx context.Context, text string, ont *types.Ontology) ([]types.ExtractedEntity, []types.ExtractedRelation, error) {
	prompt := BuildExtractionPrompt(text, ont)

	raw, err := e.client.GenerateWithModel(ctx, prompt, e.client.Model, jsonFormat, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("ollama extraction: %w", err)
	}

	var resp extractionResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		return nil, nil, fmt.Errorf("parse extraction response: %w", err)
	}

	return resp.Entities, resp.Relations, nil
}
