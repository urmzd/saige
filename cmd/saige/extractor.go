package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	agenttypes "github.com/urmzd/saige/agent/types"
	kgtypes "github.com/urmzd/saige/rag/knowledge/types"
)

// providerExtractor implements kgtypes.Extractor on top of any chat provider,
// so knowledge graph ingest works with whichever --provider the CLI resolved.
// The provider is created on first use: commands that only read the graph
// never need LLM credentials.
type providerExtractor struct {
	newProvider func() (agenttypes.Provider, error)

	once     sync.Once
	provider agenttypes.Provider
	err      error
}

func newProviderExtractor(newProvider func() (agenttypes.Provider, error)) *providerExtractor {
	return &providerExtractor{newProvider: newProvider}
}

const extractionInstructions = `Extract entities and relationships from the text below. Return ONLY valid JSON, with no prose and no code fences, in exactly this shape:
{"entities": [{"name": "...", "type": "...", "summary": "..."}],
 "relations": [{"source": "...", "target": "...", "type": "...", "fact": "..."}]}
Every relation source and target must be the name of an extracted entity.`

// extractionSchema constrains output on providers that support structured
// output. Providers without it rely on the instructions alone.
var extractionSchema = mustSchema(`{
  "type": "object",
  "required": ["entities", "relations"],
  "properties": {
    "entities": {"type": "array", "items": {
      "type": "object",
      "required": ["name", "type", "summary"],
      "properties": {"name": {"type": "string"}, "type": {"type": "string"}, "summary": {"type": "string"}}
    }},
    "relations": {"type": "array", "items": {
      "type": "object",
      "required": ["source", "target", "type", "fact"],
      "properties": {"source": {"type": "string"}, "target": {"type": "string"}, "type": {"type": "string"}, "fact": {"type": "string"}}
    }}
  }
}`)

func mustSchema(src string) *agenttypes.ParameterSchema {
	var s agenttypes.ParameterSchema
	if err := json.Unmarshal([]byte(src), &s); err != nil {
		panic("invalid extraction schema: " + err.Error())
	}
	return &s
}

// Extract implements kgtypes.Extractor.
func (e *providerExtractor) Extract(ctx context.Context, text string) ([]kgtypes.ExtractedEntity, []kgtypes.ExtractedRelation, error) {
	e.once.Do(func() { e.provider, e.err = e.newProvider() })
	if e.err != nil {
		return nil, nil, fmt.Errorf("extraction provider: %w", e.err)
	}

	msgs := []agenttypes.Message{
		agenttypes.NewUserMessage(extractionInstructions + "\n\nText:\n" + text),
	}
	var (
		ch  <-chan agenttypes.Delta
		err error
	)
	if sp, ok := e.provider.(agenttypes.StructuredOutputProvider); ok {
		ch, err = sp.ChatStreamWithSchema(ctx, msgs, nil, extractionSchema)
	} else {
		ch, err = e.provider.ChatStream(ctx, msgs, nil)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("extraction: %w", err)
	}

	raw, err := collectText(ch)
	if err != nil {
		return nil, nil, fmt.Errorf("extraction: %w", err)
	}
	return parseExtraction(raw)
}

// collectText drains a provider stream and returns its text output.
func collectText(ch <-chan agenttypes.Delta) (string, error) {
	var b strings.Builder
	var streamErr error
	for d := range ch {
		switch d := d.(type) {
		case agenttypes.TextContentDelta:
			b.WriteString(d.Content)
		case agenttypes.ErrorDelta:
			if streamErr == nil {
				streamErr = d.Error
			}
		}
	}
	return b.String(), streamErr
}

// parseExtraction decodes the model's JSON answer. It tolerates code fences
// and text around the object, and drops entities without a name and
// relations whose endpoints are missing.
func parseExtraction(raw string) ([]kgtypes.ExtractedEntity, []kgtypes.ExtractedRelation, error) {
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end < start {
		return nil, nil, errors.New("extraction: model returned no JSON object")
	}
	var resp struct {
		Entities  []kgtypes.ExtractedEntity   `json:"entities"`
		Relations []kgtypes.ExtractedRelation `json:"relations"`
	}
	if err := json.Unmarshal([]byte(raw[start:end+1]), &resp); err != nil {
		return nil, nil, fmt.Errorf("extraction: parse response: %w", err)
	}

	entities := resp.Entities[:0]
	for _, ent := range resp.Entities {
		if strings.TrimSpace(ent.Name) != "" {
			entities = append(entities, ent)
		}
	}
	relations := resp.Relations[:0]
	for _, rel := range resp.Relations {
		if strings.TrimSpace(rel.Source) != "" && strings.TrimSpace(rel.Target) != "" {
			relations = append(relations, rel)
		}
	}
	return entities, relations, nil
}
