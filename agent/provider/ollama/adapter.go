package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/provider/internal/streamcheck"
	"github.com/urmzd/saige/agent/types"
)

// Compile-time interface checks.
var (
	_ types.StructuredOutputProvider = (*Adapter)(nil)
	_ types.NamedProvider            = (*Adapter)(nil)
	_ types.ModelProvider            = (*Adapter)(nil)
	_ types.ModelSwitcher            = (*Adapter)(nil)
	_ types.CapabilityReporter       = (*Adapter)(nil)
	_ types.ContentNegotiator        = (*Adapter)(nil)
)

// Name implements types.NamedProvider.
func (a *Adapter) Name() string { return "ollama" }

// Model implements types.ModelProvider.
func (a *Adapter) Model() string { return a.Client.Model }

// WithModel implements types.ModelSwitcher: it returns a copy of the adapter
// (and its client) targeting the given model, sharing the HTTP client.
func (a *Adapter) WithModel(model string) types.Provider {
	client := *a.Client
	client.Model = model
	return &Adapter{Client: &client, toolChoice: a.toolChoice, dials: a.dials, dialPolicy: a.dialPolicy}
}

// Adapter wraps the Ollama Client and implements types.Provider.
type Adapter struct {
	Client *Client

	toolChoice *types.ToolChoice
	dials      []types.DialLayer
	dialPolicy *types.DialPolicy
}

// NewAdapter creates a new Ollama Provider adapter.
func NewAdapter(client *Client, opts ...AdapterOption) *Adapter {
	a := &Adapter{Client: client}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Validate checks explicitly configured controls. The low-level Client remains
// a wire client; use Adapter when model capability enforcement is required.
func (a *Adapter) Validate() error {
	o, err := a.clientOptions()
	if err != nil {
		return err
	}
	if err := a.Capabilities().ValidateOptions(o); err != nil {
		return err
	}
	return a.validateToolChoice()
}

// EffectiveOptions implements types.OptionsReporter: the client's think flag
// and sampling options as request options. The emulated tool choice is not
// included, since it is not a request control on the wire.
func (a *Adapter) EffectiveOptions() types.RequestOptions {
	o, _ := a.clientOptions()
	o.DialLayers, o.DialPolicy = a.dials, a.dialPolicy
	return o.Clone()
}

// clientOptions decodes the client's configured controls.
func (a *Adapter) clientOptions() (types.RequestOptions, error) {
	o := types.RequestOptions{ReasoningEnabled: a.Client.Think}
	if a.Client.ChatOptions != nil {
		// Decode the actual wire representation: maps can express zero values,
		// whereas the legacy Options struct omits its zero-valued fields.
		data, err := json.Marshal(a.Client.ChatOptions)
		if err != nil {
			return o.Clone(), a.Capabilities().OptionError("options", "cannot encode options")
		}
		var g struct {
			Temperature *float64 `json:"temperature"`
			TopP        *float64 `json:"top_p"`
			TopK        *int64   `json:"top_k"`
			Seed        *int64   `json:"seed"`
			NumPredict  *int64   `json:"num_predict"`
			Stop        []string `json:"stop"`
		}
		if err := json.Unmarshal(data, &g); err != nil {
			return o.Clone(), a.Capabilities().OptionError("options", "invalid sampling option types")
		}
		o.Temperature, o.TopP, o.Seed, o.StopSequences = g.Temperature, g.TopP, g.Seed, g.Stop
		if g.TopK != nil {
			k := float64(*g.TopK)
			o.TopK = &k
		}
		if g.NumPredict != nil && *g.NumPredict != -1 {
			o.MaxOutputTokens = g.NumPredict
		}
	}
	return o.Clone(), nil
}

// Stream implements types.Provider. A request may carry a schema and
// options together: the options apply first (tool choice and dials), then
// the schema is sent as the format constraint.
func (a *Adapter) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	if req.Options != nil {
		return a.chatStreamWithOptions(ctx, req.Messages, req.Tools, req.Schema, *req.Options)
	}
	return a.chatStream(ctx, req.Messages, req.Tools, req.Schema)
}

// SupportsSchema implements types.StructuredOutputProvider.
func (a *Adapter) SupportsSchema() bool { return true }

// SupportsOptions implements types.OptionsProvider.
func (a *Adapter) SupportsOptions() bool { return true }

func (a *Adapter) chatStream(ctx context.Context, messages []types.Message, tools []types.ToolDef, schema *types.ParameterSchema) (<-chan types.Delta, error) {
	a, err := a.compileDials(types.RequestOptions{}, tools, schema != nil)
	if err != nil {
		return nil, err
	}
	// The emulated tool choice decides which tools are sent, so the request
	// is checked against the filtered set.
	tools, err = a.filterTools(tools)
	if err != nil {
		return nil, err
	}
	if err := a.Capabilities().ValidateRequest(tools, schema != nil); err != nil {
		return nil, err
	}

	if err := a.Validate(); err != nil {
		return nil, err
	}

	oMsgs, err := toOllamaMessages(messages)
	if err != nil {
		caps := a.Capabilities()
		return nil, &types.ProviderError{Provider: caps.Provider, Model: caps.Model, Kind: types.ErrorKindPermanent, Err: err}
	}
	oTools := toOllamaTools(tools)

	var format any
	if schema != nil {
		format = parameterSchemaToMap(*schema)
	}

	rx, err := a.Client.ChatStreamWithFormat(ctx, oMsgs, oTools, format)
	if err != nil {
		return nil, classifyOllamaError(a.Client.Model, err)
	}

	return a.translateDeltas(ctx, rx, schema != nil), nil
}

// translateDeltas converts the Ollama ChatChunk stream to part deltas.
// Ollama has no content-block index, so parts are numbered in the order
// they start: thinking, text and each tool call open a new index. Thinking
// closes when text or a tool call begins, text closes when a tool call
// begins, and both close at the final chunk. Thinking carries no signature.
//
// A stream counts as complete only when a chunk with done:true arrives. A
// server error line, a client read failure, or a channel that closes early
// ends the stream with an ErrorDelta, so a partial answer is never reported
// as a clean finish (and never admitted to a response cache). A structured
// response that stopped at done_reason "length" is a truncation error, since
// its JSON is incomplete.
//
//nolint:gocyclo // a single pass over a stream or loop state; splitting it would scatter shared state
func (a *Adapter) translateDeltas(ctx context.Context, rx <-chan ChatChunk, structured bool) <-chan types.Delta {
	model := a.Client.Model
	out := make(chan types.Delta, 64)
	go func() {
		defer close(out)

		next := 0
		text, think := -1, -1
		open := func(kind types.PartKind) int {
			i := next
			next++
			out <- types.PartStart{Index: i, Kind: kind}
			return i
		}
		closeThink := func() {
			if think >= 0 {
				out <- types.PartEnd{Index: think}
				think = -1
			}
		}
		closeText := func() {
			if text >= 0 {
				out <- types.PartEnd{Index: text}
				text = -1
			}
		}

		emitted := false
		done := false
		var failure *types.ProviderError
		for chunk := range rx {
			if chunk.Err != nil {
				failure = streamcheck.StreamError("ollama", model, chunk.Err, !emitted)
				break
			}
			if chunk.Error != "" {
				failure = streamcheck.StreamError("ollama", model, fmt.Errorf("ollama stream error: %s", chunk.Error), !emitted)
				break
			}
			if chunk.Message.Thinking != "" || chunk.Message.Content != "" || len(chunk.Message.ToolCalls) > 0 {
				emitted = true
			}
			if chunk.Done {
				done = true
				closeThink()
				closeText()
				// Emit usage delta from the final chunk.
				ud := types.UsageDelta{Cumulative: true,
					PromptTokens:     chunk.PromptEvalCount,
					CompletionTokens: chunk.EvalCount,
					TotalTokens:      chunk.PromptEvalCount + chunk.EvalCount,
					ResponseModel:    chunk.Model,
				}
				if chunk.DoneReason != "" {
					ud.FinishReasons = []string{chunk.DoneReason}
				} else {
					ud.FinishReasons = []string{"stop"}
				}
				out <- ud
				if structured && types.IsTruncationFinishReason(chunk.DoneReason) {
					failure = streamcheck.Truncated("ollama", model, chunk.DoneReason, chunk.EvalCount, 0)
				}
				continue
			}

			// Reasoning from a thinking model (the think field).
			if chunk.Message.Thinking != "" {
				if think < 0 {
					think = open(types.KindThinking)
				}
				out <- types.PartDelta{Index: think, Thinking: chunk.Message.Thinking}
			}

			if chunk.Message.Content != "" {
				closeThink()
				if text < 0 {
					text = open(types.KindText)
				}
				out <- types.PartDelta{Index: text, Text: chunk.Message.Content}
			}

			// A tool call arrives whole, with decoded arguments.
			if len(chunk.Message.ToolCalls) > 0 {
				closeThink()
				closeText()
				for _, tc := range chunk.Message.ToolCalls {
					id := tc.ID
					if id == "" {
						id = types.NewID()
					}
					args := tc.Function.Arguments
					if args == nil {
						args = map[string]any{}
					}
					i := next
					next++
					out <- types.PartStart{Index: i, Kind: types.KindToolCall, ID: id, Name: tc.Function.Name}
					if raw, err := json.Marshal(args); err == nil {
						out <- types.PartDelta{Index: i, Args: string(raw)}
					}
					out <- types.PartEnd{Index: i, Part: types.ToolCallPart{ID: id, Name: tc.Function.Name, Arguments: args}}
				}
			}
		}

		// A stream that ends without done leaves its parts open, so the
		// aggregator sees them as truncated.
		if failure == nil && !done {
			cause := streamcheck.ErrIncompleteStream
			if err := ctx.Err(); err != nil {
				cause = err
			}
			failure = streamcheck.StreamError("ollama", model, cause, !emitted)
		}
		if failure == nil {
			return
		}
		select {
		case out <- types.ErrorDelta{Error: failure}:
		case <-ctx.Done():
			// The consumer may be gone; deliver only if there is room.
			select {
			case out <- types.ErrorDelta{Error: failure}:
			default:
			}
		}
	}()

	return out
}

// Capabilities implements types.CapabilityReporter. Ollama is the one provider
// whose capabilities follow the pulled weights rather than the endpoint, so an
// unrecognised model resolves to the conservative baseline (Known false) and
// callers that must fail closed can see that.
//
// A model that calls tools also reports CapToolChoice, because the adapter
// emulates none and named choices by filtering the tools it sends (see
// WithToolChoice). Required cannot be emulated and is still rejected before
// any request.
func (a *Adapter) Capabilities() types.ModelCapabilities {
	caps := catalog.MustLookup("ollama", a.Client.Model)
	if caps.Supports(types.CapTools) {
		caps = caps.With(types.CapToolChoice)
	}
	return caps
}

// ContentSupport implements types.ContentNegotiator.
// The Ollama images field carries JPEG and PNG, but only a vision model can
// read them: Capabilities is the model-aware answer, this is the wire-format
// one.
func (a *Adapter) ContentSupport() types.ContentSupport {
	return types.ContentSupport{
		NativeTypes: map[types.MediaType]bool{
			types.MediaJPEG: true,
			types.MediaPNG:  true,
		},
	}
}

// ── Convenience methods (not part of Provider) ──────────────────────

// Generate delegates to the underlying client.
func (a *Adapter) Generate(ctx context.Context, prompt string) (string, error) {
	return a.Client.Generate(ctx, prompt)
}

// GenerateWithModel delegates to the underlying client.
func (a *Adapter) GenerateWithModel(ctx context.Context, prompt, model string, format, options any) (string, error) {
	return a.Client.GenerateWithModel(ctx, prompt, model, format, options)
}

// GenerateStream delegates to the underlying client.
func (a *Adapter) GenerateStream(ctx context.Context, prompt string) (<-chan string, error) {
	return a.Client.GenerateStream(ctx, prompt)
}

// Embed delegates to the underlying client.
func (a *Adapter) Embed(ctx context.Context, text string) ([]float32, error) {
	return a.Client.Embed(ctx, text)
}

// ── Conversion helpers ──────────────────────────────────────────────

func toOllamaTools(defs []types.ToolDef) []Tool {
	out := make([]Tool, len(defs))
	for i, d := range defs {
		props := make(map[string]ToolProperty, len(d.Parameters.Properties))
		for k, v := range d.Parameters.Properties {
			props[k] = convertProperty(v)
		}
		out[i] = Tool{
			Type: "function",
			Function: ToolFunction{
				Name:        d.Name,
				Description: d.Description,
				Parameters: ToolFunctionParams{
					Type:       d.Parameters.Type,
					Required:   d.Parameters.Required,
					Properties: props,
				},
			},
		}
	}
	return out
}

// convertProperty recursively converts a types.PropertyDef to an Ollama ToolProperty.
func convertProperty(p types.PropertyDef) ToolProperty {
	tp := ToolProperty{
		Nullable:    p.Nullable,
		Type:        p.Type,
		Description: p.Description,
		Enum:        p.Enum,
		Required:    p.Required,
		Default:     p.Default,
	}
	if p.Items != nil {
		items := convertProperty(*p.Items)
		tp.Items = &items
	}
	if len(p.Properties) > 0 {
		tp.Properties = make(map[string]ToolProperty, len(p.Properties))
		for k, v := range p.Properties {
			tp.Properties[k] = convertProperty(v)
		}
	}
	return tp
}

// parameterSchemaToMap converts a ParameterSchema to a map for the Ollama format field.
func parameterSchemaToMap(ps types.ParameterSchema) map[string]any {
	schema := map[string]any{"type": ps.Type}
	if len(ps.Required) > 0 {
		schema["required"] = ps.Required
	}
	if len(ps.Properties) > 0 {
		props := make(map[string]any, len(ps.Properties))
		for k, v := range ps.Properties {
			props[k] = propertyDefToMap(v)
		}
		schema["properties"] = props
	}
	return schema
}

func propertyDefToMap(p types.PropertyDef) map[string]any {
	return p.JSONSchema()
}

// classifyOllamaError maps a request error to a ProviderError. A non-200
// response is classified by status, body, and Retry-After. Anything else is a
// transport failure (refused, reset, timeout), which is transient.
func classifyOllamaError(model string, err error) *types.ProviderError {
	var statusErr *StatusError
	if errors.As(err, &statusErr) {
		return streamcheck.HTTPError("ollama", model, statusErr.Code, statusErr.Header, statusErr.Body, err)
	}
	return streamcheck.StreamError("ollama", model, err, true)
}
