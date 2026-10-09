package ollama

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/types"
)

var (
	_ catalog.ModelLister = (*Adapter)(nil)
	_ catalog.ModelLister = (*Client)(nil)
)

// AdapterOption configures an Adapter.
type AdapterOption func(*Adapter)

// WithToolChoice emulates a tool choice, since the chat API has no
// tool_choice field. It works by filtering the tools sent:
//
//   - none sends no tools, so the model cannot call one;
//   - a named tool sends only that tool, so the model can call nothing else,
//     though it may still answer without calling it;
//   - auto sends every tool.
//
// Required cannot be emulated by filtering and is rejected by Validate.
func WithToolChoice(c types.ToolChoice) AdapterOption {
	return func(a *Adapter) { a.toolChoice = &c }
}

// validateToolChoice rejects choices the emulation cannot honor.
func (a *Adapter) validateToolChoice() error {
	c := a.toolChoice
	if c == nil {
		return nil
	}
	caps := a.Capabilities()
	if err := c.Validate(nil); err != nil {
		return &types.ProviderError{Provider: caps.Provider, Model: caps.Model, Kind: types.ErrorKindPermanent, Err: err}
	}
	if c.Mode == types.ToolChoiceRequired {
		return caps.OptionError("tool_choice", "required cannot be enforced: ollama has no tool_choice, and filtering cannot force a call")
	}
	return nil
}

// filterTools applies the emulated tool choice to the offered tools.
func (a *Adapter) filterTools(tools []types.ToolDef) ([]types.ToolDef, error) {
	c := a.toolChoice
	if c == nil || len(tools) == 0 {
		return tools, nil
	}
	if err := c.Validate(tools); err != nil {
		caps := a.Capabilities()
		return nil, &types.ProviderError{Provider: caps.Provider, Model: caps.Model, Kind: types.ErrorKindPermanent, Err: err}
	}
	switch c.Mode {
	case types.ToolChoiceNone:
		return nil, nil
	case types.ToolChoiceNamed:
		for _, t := range tools {
			if t.Name == c.Name {
				return []types.ToolDef{t}, nil
			}
		}
	}
	return tools, nil
}

// ListModels implements catalog.ModelLister by delegating to the client.
func (a *Adapter) ListModels(ctx context.Context) ([]catalog.RemoteModel, error) {
	return a.Client.ListModels(ctx)
}

// tagsResponse is the body of GET /api/tags.
type tagsResponse struct {
	Models []struct {
		Name       string    `json:"name"`
		Model      string    `json:"model"`
		ModifiedAt time.Time `json:"modified_at"`
		Size       int64     `json:"size"`
		Details    struct {
			Family   string   `json:"family"`
			Families []string `json:"families"`
		} `json:"details"`
	} `json:"models"`
}

// ListModels implements catalog.ModelLister with GET /api/tags: the models
// pulled on this host. Embedding is set for models the catalog declares as
// embedders, whose name contains "embed", or whose family is a BERT encoder.
// Created is the local modification time, which is when the weights were
// pulled, not when the model was released.
func (c *Client) ListModels(ctx context.Context) ([]catalog.RemoteModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.Host, "/")+"/api/tags", nil)
	if err != nil {
		return nil, classifyOllamaError(c.Model, err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, classifyOllamaError(c.Model, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, classifyOllamaError(c.Model, statusError(resp))
	}
	defer func() { _ = resp.Body.Close() }()
	var tags tagsResponse
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return nil, classifyOllamaError(c.Model, fmt.Errorf("ollama: decode /api/tags: %w", err))
	}
	out := make([]catalog.RemoteModel, 0, len(tags.Models))
	for _, m := range tags.Models {
		id := m.Name
		if id == "" {
			id = m.Model
		}
		embedding := catalog.MustLookup("ollama", id).Supports(types.CapEmbeddings) ||
			strings.Contains(strings.ToLower(id), "embed")
		for _, f := range append([]string{m.Details.Family}, m.Details.Families...) {
			embedding = embedding || strings.Contains(strings.ToLower(f), "bert")
		}
		out = append(out, catalog.RemoteModel{ID: id, Created: m.ModifiedAt, SizeBytes: m.Size, Embedding: embedding})
	}
	return out, nil
}
