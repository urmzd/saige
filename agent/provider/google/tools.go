package google

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/types"
	"google.golang.org/genai"
)

var _ catalog.ModelLister = (*Adapter)(nil)

// WithToolChoice constrains whether and which function the model calls. It
// maps to ToolConfig.FunctionCallingConfig: auto is AUTO, none is NONE,
// required is ANY, and a named tool is ANY restricted to that function. Modes
// other than auto require CapToolChoice. The choice applies to requests that
// offer function tools; server tools alone carry no tool config.
func WithToolChoice(c types.ToolChoice) Option {
	return func(a *Adapter) { a.toolChoice = &c }
}

// checkToolChoice validates the configured choice against the offered tools.
func (a *Adapter) checkToolChoice(tools []types.ToolDef) error {
	if a.toolChoice == nil || len(tools) == 0 {
		return nil
	}
	return a.Capabilities().ValidateToolChoice(a.toolChoice, tools)
}

// toolConfig encodes the configured choice, or nil when none is set.
func (a *Adapter) toolConfig() *genai.ToolConfig {
	c := a.toolChoice
	if c == nil {
		return nil
	}
	fc := &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAuto}
	switch c.Mode {
	case types.ToolChoiceNone:
		fc.Mode = genai.FunctionCallingConfigModeNone
	case types.ToolChoiceRequired:
		fc.Mode = genai.FunctionCallingConfigModeAny
	case types.ToolChoiceNamed:
		fc.Mode = genai.FunctionCallingConfigModeAny
		fc.AllowedFunctionNames = []string{c.Name}
	}
	return &genai.ToolConfig{FunctionCallingConfig: fc}
}

// serverToolState pairs Gemini's server tool parts into call and result
// deltas over one stream.
type serverToolState struct {
	// pendingCode holds the IDs of code calls still waiting for a result,
	// oldest first. Gemini usually omits part IDs, and a result follows its
	// code in order.
	pendingCode []string
	// grounding is the latest metadata that carried search queries.
	grounding *genai.GroundingMetadata
}

// codeCall reports generated code as a server tool call.
func (s *serverToolState) codeCall(c *genai.ExecutableCode) types.ServerToolCallDelta {
	id := c.ID
	if id == "" {
		id = types.NewID()
	}
	s.pendingCode = append(s.pendingCode, id)
	input := map[string]any{"code": c.Code}
	if c.Language != "" {
		input["language"] = string(c.Language)
	}
	return types.ServerToolCallDelta{ID: id, Kind: types.ServerToolCodeExecution, Name: "code_execution", Input: input}
}

// codeResult reports a code execution outcome and pairs it with its call.
func (s *serverToolState) codeResult(r *genai.CodeExecutionResult) types.ServerToolResultDelta {
	id := r.ID
	switch i := slices.Index(s.pendingCode, id); {
	case id != "" && i >= 0:
		s.pendingCode = slices.Delete(s.pendingCode, i, i+1)
	case id == "" && len(s.pendingCode) > 0:
		id, s.pendingCode = s.pendingCode[0], s.pendingCode[1:]
	case id == "":
		id = types.NewID()
	}
	raw, _ := json.Marshal(r)
	return types.ServerToolResultDelta{
		ID: id, Kind: types.ServerToolCodeExecution, Text: r.Output, Result: raw,
		IsError: r.Outcome != "" && r.Outcome != genai.OutcomeOK,
	}
}

// observe keeps the latest grounding metadata that names search queries.
// Gemini can repeat or extend it across chunks, so the search is reported
// once, at the end of the stream.
func (s *serverToolState) observe(md *genai.GroundingMetadata) {
	if md != nil && len(md.WebSearchQueries) > 0 {
		s.grounding = md
	}
}

// search reports the observed search as a call with its queries and a result
// listing the sources found. It returns nothing when no search ran.
func (s *serverToolState) search() []types.Delta {
	md := s.grounding
	if md == nil {
		return nil
	}
	s.grounding = nil
	id := types.NewID()
	queries := make([]any, len(md.WebSearchQueries))
	for i, q := range md.WebSearchQueries {
		queries[i] = q
	}
	var lines []string
	for _, chunk := range md.GroundingChunks {
		if chunk != nil && chunk.Web != nil && chunk.Web.URI != "" {
			lines = append(lines, strings.TrimSpace(chunk.Web.Title+" "+chunk.Web.URI))
		}
	}
	raw, _ := json.Marshal(md.GroundingChunks)
	return []types.Delta{
		types.ServerToolCallDelta{ID: id, Kind: types.ServerToolWebSearch, Name: "google_search", Input: map[string]any{"queries": queries}},
		types.ServerToolResultDelta{ID: id, Kind: types.ServerToolWebSearch, Text: strings.Join(lines, "\n"), Result: raw},
	}
}

// ListModels implements catalog.ModelLister. IDs drop the resource prefix
// ("models/gemini-2.5-flash" becomes "gemini-2.5-flash") so they match the
// names NewAdapter and the catalog use.
func (a *Adapter) ListModels(ctx context.Context) ([]catalog.RemoteModel, error) {
	var out []catalog.RemoteModel
	for m, err := range a.client.Models.All(ctx) {
		if err != nil {
			return nil, classifyGoogleError(a.model, err, true)
		}
		if m == nil {
			continue
		}
		id := m.Name
		if i := strings.LastIndex(id, "/"); i >= 0 {
			id = id[i+1:]
		}
		out = append(out, catalog.RemoteModel{
			ID:              id,
			DisplayName:     m.DisplayName,
			ContextWindow:   int(m.InputTokenLimit),
			MaxOutputTokens: int(m.OutputTokenLimit),
			Embedding: slices.Contains(m.SupportedActions, "embedContent") &&
				!slices.Contains(m.SupportedActions, "generateContent"),
		})
	}
	return out, nil
}
