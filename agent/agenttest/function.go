package agenttest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/types"
)

// Request is what a ModelFunc sees about one call besides the messages and
// tools.
type Request struct {
	// Index numbers the calls the model received, from 0.
	Index int
	// Options are the per-request controls the caller sent, or nil for a
	// call that carried none (Stream or Stream with a schema).
	Options *types.RequestOptions
	// Effective is what an adapter would send: the model's configured
	// Options with the request's on top and every dial compiled against the
	// model's capabilities. It carries no dials.
	Effective types.RequestOptions
	// Dials reports how the call's dials compiled. Nil when it had none.
	Dials *types.DialReport
	// Schema is the response schema of a Stream with a schema call.
	Schema *types.ParameterSchema
}

// Response is one model turn, streamed by FunctionModel in the order a
// provider produces it: thinking, text, tool calls, usage, then a stream
// error.
type Response struct {
	// Thinking is streamed as one thinking block before the text.
	Thinking string
	// Text is streamed as one text block. TextChunks, when set, replaces it
	// and streams each element as its own content delta.
	Text       string
	TextChunks []string
	// ToolCalls are streamed in order, each with its arguments as a JSON
	// fragment. An empty ID is filled in as "call_<index>_<n>".
	ToolCalls []types.ToolCallPart
	// Usage is sent after the content. FinishReason, when set, is added to
	// its FinishReasons, so a turn can end with "max_tokens" or
	// "content_filter" without usage figures.
	Usage        *types.UsageDelta
	FinishReason string
	// Deltas are sent as they are after the usage, for anything the fields
	// above do not cover, such as a CitationDelta.
	Deltas []types.Delta
	// StreamErr fails the stream after the content, as a connection that
	// drops mid-response does. An error the ModelFunc returns instead fails
	// the call before anything streams.
	StreamErr error
}

// ModelFunc produces the response to one model call.
type ModelFunc func(ctx context.Context, messages []types.Message, tools []types.ToolDef, req Request) (Response, error)

// FunctionModel is a provider whose responses come from a Go function, for
// tests that need a reply computed from the request rather than a fixed
// script. It is safe for concurrent use.
//
// It implements the optional provider interfaces the agent loop looks for:
// types.OptionsProvider, types.StructuredOutputProvider,
// types.CapabilityReporter, types.OptionsReporter, types.NamedProvider and
// types.ModelProvider. Its capabilities come from Caps, then from
// the catalog row CatalogModel names, then DefaultCapabilities. With
// Caps or CatalogModel set it checks every request against them as
// an adapter does, before calling Fn, so a test can exercise capability
// gating without a vendor API.
type FunctionModel struct {
	// Fn computes each response. A nil Fn answers every call with an empty
	// text turn.
	Fn ModelFunc
	// ProviderName and ModelName identify the model. They default to
	// "function" and "function-model".
	ProviderName string
	ModelName    string
	// Options are the configured request options, dials included, that
	// every call starts from.
	Options types.RequestOptions
	// Caps declares what the model supports.
	Caps *types.ModelCapabilities
	// CatalogModel names a catalog row as "provider/model", such as
	// "anthropic/claude-haiku-4-5", whose capabilities the model declares
	// when Caps is nil.
	CatalogModel string

	mu    sync.Mutex
	calls []FunctionCall
}

// FunctionCall is one request a FunctionModel received.
type FunctionCall struct {
	Messages []types.Message
	Tools    []types.ToolDef
	Request  Request
}

var (
	_ types.OptionsProvider          = (*FunctionModel)(nil)
	_ types.StructuredOutputProvider = (*FunctionModel)(nil)
	_ types.CapabilityReporter       = (*FunctionModel)(nil)
	_ types.OptionsReporter          = (*FunctionModel)(nil)
	_ types.NamedProvider            = (*FunctionModel)(nil)
	_ types.ModelProvider            = (*FunctionModel)(nil)
)

// DefaultCapabilities is what a FunctionModel without a declaration
// reports: a model that streams, calls tools, accepts a tool choice,
// enforces a response schema natively and takes the common sampling
// controls. Known is false, so nothing treats it as an exact declaration.
func DefaultCapabilities() types.ModelCapabilities {
	caps := map[types.Capability]bool{}
	for _, c := range []types.Capability{
		types.CapStreaming, types.CapSystemPrompt, types.CapTools, types.CapParallelTools,
		types.CapToolChoice, types.CapStructuredOutput, types.CapTemperature, types.CapTopP,
		types.CapMaxOutputTokens, types.CapStopSequences, types.CapSeed,
	} {
		caps[c] = true
	}
	return types.ModelCapabilities{
		Provider:         "function",
		Model:            "function-model",
		Caps:             caps,
		StructuredOutput: types.StructuredOutputNative,
	}
}

// Name implements types.NamedProvider.
func (m *FunctionModel) Name() string {
	if m.ProviderName != "" {
		return m.ProviderName
	}
	return "function"
}

// Model implements types.ModelProvider.
func (m *FunctionModel) Model() string {
	if m.ModelName != "" {
		return m.ModelName
	}
	if _, model, ok := strings.Cut(m.CatalogModel, "/"); ok {
		return model
	}
	return "function-model"
}

// Capabilities implements types.CapabilityReporter.
func (m *FunctionModel) Capabilities() types.ModelCapabilities {
	caps, _ := m.declared()
	return caps
}

// declared returns the model's capabilities and whether they were declared
// rather than defaulted.
func (m *FunctionModel) declared() (types.ModelCapabilities, bool) {
	if m.Caps != nil {
		return *m.Caps, true
	}
	if provider, model, ok := strings.Cut(m.CatalogModel, "/"); ok {
		caps, _ := catalog.Lookup(types.ProviderName(provider), model)
		return caps, true
	}
	caps := DefaultCapabilities()
	caps.Provider, caps.Model = m.Name(), m.Model()
	return caps, false
}

// EffectiveOptions implements types.OptionsReporter.
func (m *FunctionModel) EffectiveOptions() types.RequestOptions { return m.Options.Clone() }

// Stream implements types.Provider.
func (m *FunctionModel) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	return m.stream(ctx, req.Messages, req.Tools, req.Options, req.Schema)
}

// SupportsOptions implements types.OptionsProvider.
func (m *FunctionModel) SupportsOptions() bool { return true }

// SupportsSchema implements types.StructuredOutputProvider.
func (m *FunctionModel) SupportsSchema() bool { return true }

// CallCount returns how many requests the model has received.
func (m *FunctionModel) CallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// Requests returns a copy of the recorded requests, including those the
// capability check rejected.
func (m *FunctionModel) Requests() []FunctionCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]FunctionCall(nil), m.calls...)
}

func (m *FunctionModel) stream(ctx context.Context, messages []types.Message, tools []types.ToolDef, opts *types.RequestOptions, schema *types.ParameterSchema) (<-chan types.Delta, error) {
	caps, strict := m.declared()
	req := Request{Options: cloneOptions(opts), Schema: schema}
	all := m.Options.Clone()
	if opts != nil {
		all = all.Merge(*opts)
	}
	eff, rep, err := types.CompileOptions(caps, all, types.DialContext{Tools: len(tools) > 0, Schema: schema != nil})
	req.Effective, req.Dials = eff, rep
	if err == nil && strict {
		err = validate(caps, eff, tools, schema)
	}

	m.mu.Lock()
	req.Index = len(m.calls)
	m.calls = append(m.calls, FunctionCall{
		Messages: append([]types.Message(nil), messages...),
		Tools:    append([]types.ToolDef(nil), tools...),
		Request:  req,
	})
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}

	var resp Response
	if m.Fn != nil {
		if resp, err = m.Fn(ctx, messages, tools, req); err != nil {
			return nil, err
		}
	}
	deltas := resp.deltas(req.Index)
	ch := make(chan types.Delta, len(deltas))
	for _, d := range deltas {
		ch <- d
	}
	close(ch)
	return ch, nil
}

// validate rejects a request the declared model cannot serve, as adapters
// do before any network I/O.
func validate(caps types.ModelCapabilities, eff types.RequestOptions, tools []types.ToolDef, schema *types.ParameterSchema) error {
	if err := caps.ValidateRequest(tools, schema != nil); err != nil {
		return err
	}
	if err := caps.ValidateOptions(eff); err != nil {
		return err
	}
	return caps.ValidateToolChoice(eff.ToolChoice, tools)
}

func cloneOptions(o *types.RequestOptions) *types.RequestOptions {
	if o == nil {
		return nil
	}
	c := o.Clone()
	return &c
}

// deltas renders the response as a provider stream: thinking, then text,
// then tool calls, at consecutive part indices.
func (r Response) deltas(index int) []types.Delta {
	var out []types.Delta
	next := 0
	if r.Thinking != "" {
		out = append(out, types.PartStart{Index: next, Kind: types.KindThinking},
			types.PartDelta{Index: next, Thinking: r.Thinking}, types.PartEnd{Index: next})
		next++
	}
	chunks := r.TextChunks
	if len(chunks) == 0 && r.Text != "" {
		chunks = []string{r.Text}
	}
	if len(chunks) > 0 {
		out = append(out, types.PartStart{Index: next, Kind: types.KindText})
		for _, c := range chunks {
			if c != "" {
				out = append(out, types.PartDelta{Index: next, Text: c})
			}
		}
		out = append(out, types.PartEnd{Index: next})
		next++
	}
	for i, tc := range r.ToolCalls {
		id := tc.ID
		if id == "" {
			id = fmt.Sprintf("call_%d_%d", index, i)
		}
		out = append(out, types.PartStart{Index: next, Kind: types.KindToolCall, ID: id, Name: tc.Name})
		if tc.ArgumentsError != "" {
			out = append(out, types.PartEnd{Index: next, Part: types.ToolCallPart{ID: id, Name: tc.Name, ArgumentsError: tc.ArgumentsError}})
			next++
			continue
		}
		args := tc.Arguments
		if args == nil {
			args = map[string]any{}
		}
		raw, _ := json.Marshal(args)
		out = append(out,
			types.PartDelta{Index: next, Args: string(raw)},
			types.PartEnd{Index: next, Part: types.ToolCallPart{ID: id, Name: tc.Name, Arguments: args}})
		next++
	}
	if r.Usage != nil || r.FinishReason != "" {
		var u types.UsageDelta
		if r.Usage != nil {
			u = *r.Usage
			u.FinishReasons = append([]string(nil), u.FinishReasons...)
		}
		if r.FinishReason != "" {
			u.FinishReasons = append(u.FinishReasons, r.FinishReason)
		}
		if u.TotalTokens == 0 {
			u.TotalTokens = u.PromptTokens + u.CompletionTokens
		}
		out = append(out, u)
	}
	out = append(out, r.Deltas...)
	if r.StreamErr != nil {
		out = append(out, types.ErrorDelta{Error: r.StreamErr})
	}
	return out
}
