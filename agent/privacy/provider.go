package privacy

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/urmzd/saige/agent/types"
)

// Provider is a decorator that tokenizes every request before it leaves the
// process and restores placeholders in the response. Use it when the host
// only needs to keep personal data away from the model vendor; the tree and
// telemetry then hold real values. Use ToolRedactor when they must not.
//
// Outgoing text, tool results, and tool call arguments are tokenized.
// Thinking blocks are left as they are in both directions: their signatures
// cover the text the model produced, and a changed block is rejected. File
// bytes are not inspected.
//
// Streamed text and tool argument fragments are restored with a hold-back,
// so a placeholder split across fragments is restored whole. Held text is
// released at the end of each block, on completion, and on error.
type Provider struct {
	Inner types.Provider
	Vault Vault
}

// NewProvider wraps inner with v.
func NewProvider(inner types.Provider, v Vault) *Provider {
	return &Provider{Inner: inner, Vault: v}
}

// Name implements types.NamedProvider.
func (p *Provider) Name() string { return "privacy(" + types.ProviderName(p.Inner) + ")" }

// Model implements types.ModelProvider.
func (p *Provider) Model() string { return types.ProviderModel(p.Inner) }

// WithModel implements types.ModelSwitcher. The vault is shared, so
// placeholders keep their meaning after a model switch.
func (p *Provider) WithModel(model string) types.Provider {
	return &Provider{Inner: types.ProviderWithModel(p.Inner, model), Vault: p.Vault}
}

// NewSession implements types.SessionProvider. The session shares the vault.
func (p *Provider) NewSession() types.Provider {
	return &Provider{Inner: types.NewProviderSession(p.Inner), Vault: p.Vault}
}

// ContentSupport implements types.ContentNegotiator.
func (p *Provider) ContentSupport() types.ContentSupport {
	return types.ProviderContentSupport(p.Inner)
}

// Capabilities implements types.CapabilityReporter. Controls that need
// request options are dropped when the inner provider cannot receive them.
func (p *Provider) Capabilities() types.ModelCapabilities {
	caps, _ := types.ProviderCapabilities(p.Inner)
	if _, ok := p.Inner.(types.OptionsProvider); !ok {
		caps = caps.Without(types.CapToolChoice, types.CapParallelToolControl)
	}
	return caps
}

// Unwrap returns the inner provider.
func (p *Provider) Unwrap() types.Provider { return p.Inner }

// Close implements types.Closer.
func (p *Provider) Close() error { return types.CloseProvider(p.Inner) }

// ChatStream implements types.Provider.
func (p *Provider) ChatStream(ctx context.Context, messages []types.Message, tools []types.ToolDef) (<-chan types.Delta, error) {
	return p.call(ctx, messages, func(msgs []types.Message) (<-chan types.Delta, error) {
		return p.Inner.ChatStream(ctx, msgs, tools)
	})
}

// ChatStreamWithSchema implements types.StructuredOutputProvider. A schema
// the inner provider cannot enforce is rejected, not dropped.
func (p *Provider) ChatStreamWithSchema(ctx context.Context, messages []types.Message, tools []types.ToolDef, schema *types.ParameterSchema) (<-chan types.Delta, error) {
	sp, ok := p.Inner.(types.StructuredOutputProvider)
	if !ok {
		if schema != nil {
			return nil, p.unsupported("a response schema")
		}
		return p.ChatStream(ctx, messages, tools)
	}
	return p.call(ctx, messages, func(msgs []types.Message) (<-chan types.Delta, error) {
		return sp.ChatStreamWithSchema(ctx, msgs, tools, schema)
	})
}

// ChatStreamWithOptions implements types.OptionsProvider. Options the inner
// provider cannot receive are rejected, not dropped.
func (p *Provider) ChatStreamWithOptions(ctx context.Context, messages []types.Message, tools []types.ToolDef, opts types.RequestOptions) (<-chan types.Delta, error) {
	op, ok := p.Inner.(types.OptionsProvider)
	if !ok {
		return nil, p.unsupported("request options")
	}
	return p.call(ctx, messages, func(msgs []types.Message) (<-chan types.Delta, error) {
		return op.ChatStreamWithOptions(ctx, msgs, tools, opts)
	})
}

func (p *Provider) unsupported(what string) error {
	return &types.ProviderError{
		Provider: types.ProviderName(p.Inner),
		Model:    types.ProviderModel(p.Inner),
		Kind:     types.ErrorKindPermanent,
		Err:      fmt.Errorf("%w: provider %q does not accept %s", types.ErrInvalidModelConfig, types.ProviderName(p.Inner), what),
	}
}

func (p *Provider) call(ctx context.Context, messages []types.Message, send func([]types.Message) (<-chan types.Delta, error)) (<-chan types.Delta, error) {
	msgs, err := TokenizeMessages(ctx, p.Vault, messages)
	if err != nil {
		// Fail closed: a request that could not be redacted is not sent.
		return nil, fmt.Errorf("privacy: tokenize request: %w", err)
	}
	in, err := send(msgs)
	if err != nil {
		return nil, err
	}
	return restoreStream(ctx, p.Vault, in), nil
}

// TokenizeMessages returns a copy of messages with text, tool results, and
// tool call arguments tokenized. The input is not modified.
func TokenizeMessages(ctx context.Context, v Vault, messages []types.Message) ([]types.Message, error) {
	out := make([]types.Message, len(messages))
	for i, m := range messages {
		var err error
		switch msg := m.(type) {
		case types.SystemMessage:
			c := make([]types.SystemContent, len(msg.Content))
			for j, part := range msg.Content {
				var t any
				if t, err = tokenizeContent(ctx, v, part); err != nil {
					return nil, err
				}
				c[j] = t.(types.SystemContent)
			}
			out[i] = types.SystemMessage{Content: c}
		case types.UserMessage:
			c := make([]types.UserContent, len(msg.Content))
			for j, part := range msg.Content {
				var t any
				if t, err = tokenizeContent(ctx, v, part); err != nil {
					return nil, err
				}
				c[j] = t.(types.UserContent)
			}
			out[i] = types.UserMessage{Content: c}
		case types.AssistantMessage:
			c := make([]types.AssistantContent, len(msg.Content))
			for j, part := range msg.Content {
				var t any
				if t, err = tokenizeContent(ctx, v, part); err != nil {
					return nil, err
				}
				c[j] = t.(types.AssistantContent)
			}
			out[i] = types.AssistantMessage{Content: c}
		default:
			out[i] = m
		}
	}
	return out, nil
}

func tokenizeContent(ctx context.Context, v Vault, part any) (any, error) {
	switch c := part.(type) {
	case types.TextContent:
		text, err := v.Tokenize(ctx, c.Text)
		return types.TextContent{Text: text}, err
	case types.ToolResultContent:
		res, err := tokenizeResult(ctx, v, types.ToolResult{Text: c.Text, Blocks: c.Blocks})
		if err != nil {
			return nil, err
		}
		c.Text, c.Blocks = res.Text, res.Blocks
		return c, nil
	case types.ToolUseContent:
		args, err := tokenizeValue(ctx, v, c.Arguments)
		if err != nil {
			return nil, err
		}
		if c.Arguments != nil {
			c.Arguments = args.(map[string]any)
		}
		return c, nil
	default:
		return part, nil
	}
}

func tokenizeValue(ctx context.Context, v Vault, value any) (any, error) {
	switch x := value.(type) {
	case string:
		return v.Tokenize(ctx, x)
	case map[string]any:
		if x == nil {
			return x, nil
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			t, err := tokenizeValue(ctx, v, e)
			if err != nil {
				return nil, err
			}
			out[k] = t
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			t, err := tokenizeValue(ctx, v, e)
			if err != nil {
				return nil, err
			}
			out[i] = t
		}
		return out, nil
	default:
		return value, nil
	}
}

// escapedRestorer is implemented by vaults that can restore inside encoded
// text, such as MemoryVault.
type escapedRestorer interface {
	RestoreEscaped(text string, escape func(string) string) string
}

// jsonEscape encodes s as the inside of a JSON string.
func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

// restoreStream forwards in, restoring placeholders in text and tool call
// arguments. Held text is flushed before each block end, before the final
// delta, and when in closes.
//
//nolint:gocyclo // a single pass over a stream or loop state; splitting it would scatter shared state
func restoreStream(ctx context.Context, v Vault, in <-chan types.Delta) <-chan types.Delta {
	out := make(chan types.Delta)
	go func() {
		defer close(out)
		send := func(d types.Delta) bool {
			select {
			case out <- d:
				return true
			case <-ctx.Done():
				go func() {
					for range in {
					}
				}()
				return false
			}
		}
		restoreArgs := func(s string) string { return s }
		if er, ok := v.(escapedRestorer); ok {
			restoreArgs = func(s string) string { return er.RestoreEscaped(s, jsonEscape) }
		}
		var text *StreamRestorer
		args := map[string]*StreamRestorer{}
		lastCall := ""
		flushText := func() bool {
			if text == nil {
				return true
			}
			rest := text.Flush()
			text = nil
			return rest == "" || send(types.TextContentDelta{Content: rest})
		}
		flushArgs := func(id string) bool {
			r, ok := args[id]
			if !ok {
				return true
			}
			delete(args, id)
			rest := r.Flush()
			return rest == "" || send(types.ToolCallArgumentDelta{ID: id, Content: rest})
		}
		flushAll := func() bool {
			if !flushText() {
				return false
			}
			for id := range args {
				if !flushArgs(id) {
					return false
				}
			}
			return true
		}
		for d := range in {
			ok := true
			switch x := d.(type) {
			case types.TextContentDelta:
				if text == nil {
					text = NewStreamRestorer(v.Restore)
				}
				if s := text.Write(x.Content); s != "" {
					ok = send(types.TextContentDelta{Content: s})
				}
			case types.TextEndDelta:
				ok = flushText() && send(x)
			case types.ToolCallStartDelta:
				lastCall = x.ID
				ok = send(x)
			case types.ToolCallArgumentDelta:
				id := x.ID
				if id == "" {
					id = lastCall
				}
				r, found := args[id]
				if !found {
					r = NewStreamRestorer(restoreArgs)
					args[id] = r
				}
				if s := r.Write(x.Content); s != "" {
					x.Content = s
					ok = send(x)
				}
			case types.ToolCallEndDelta:
				id := x.ID
				if id == "" {
					id = lastCall
				}
				ok = flushArgs(id)
				if ok && x.Arguments != nil {
					x.Arguments = restoreValue(v, x.Arguments).(map[string]any)
				}
				ok = ok && send(x)
			case types.ErrorDelta, types.DoneDelta:
				ok = flushAll() && send(d)
			default:
				ok = send(d)
			}
			if !ok {
				return
			}
		}
		flushAll()
	}()
	return out
}

// EffectiveOptions implements types.OptionsReporter by forwarding to the
// inner provider.
func (p *Provider) EffectiveOptions() types.RequestOptions {
	o, _ := types.ProviderEffectiveOptions(p.Inner)
	return o
}
