package privacy

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

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
	if !types.AcceptsOptions(p.Inner) {
		caps = caps.Without(types.CapToolChoice, types.CapParallelToolControl)
	}
	return caps
}

// Unwrap returns the inner provider.
func (p *Provider) Unwrap() types.Provider { return p.Inner }

// Close implements types.Closer.
func (p *Provider) Close() error { return types.CloseProvider(p.Inner) }

// Stream implements types.Provider. A schema or options the inner provider
// cannot receive are rejected, not dropped.
func (p *Provider) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	if req.Schema != nil && !types.AcceptsSchema(p.Inner) {
		return nil, p.unsupported("a response schema")
	}
	if req.Options != nil && !types.AcceptsOptions(p.Inner) {
		return nil, p.unsupported("request options")
	}
	return p.call(ctx, req.Messages, func(msgs []types.Message) (<-chan types.Delta, error) {
		r := req
		r.Messages = msgs
		return p.Inner.Stream(ctx, r)
	})
}

// SupportsSchema implements types.StructuredOutputProvider.
func (p *Provider) SupportsSchema() bool { return true }

// SupportsOptions implements types.OptionsProvider.
func (p *Provider) SupportsOptions() bool { return true }

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
			c := make([]types.SystemPart, len(msg.Parts))
			for j, part := range msg.Parts {
				var t any
				if t, err = tokenizeContent(ctx, v, part); err != nil {
					return nil, err
				}
				c[j] = t.(types.SystemPart)
			}
			out[i] = types.SystemMessage{Parts: c}
		case types.UserMessage:
			c := make([]types.UserPart, len(msg.Parts))
			for j, part := range msg.Parts {
				var t any
				if t, err = tokenizeContent(ctx, v, part); err != nil {
					return nil, err
				}
				c[j] = t.(types.UserPart)
			}
			out[i] = types.UserMessage{Parts: c}
		case types.AssistantMessage:
			c := make([]types.AssistantPart, len(msg.Parts))
			for j, part := range msg.Parts {
				var t any
				if t, err = tokenizeContent(ctx, v, part); err != nil {
					return nil, err
				}
				c[j] = t.(types.AssistantPart)
			}
			out[i] = types.AssistantMessage{Parts: c}
		default:
			out[i] = m
		}
	}
	return out, nil
}

func tokenizeContent(ctx context.Context, v Vault, part any) (any, error) {
	switch c := part.(type) {
	case types.TextPart:
		text, err := v.Tokenize(ctx, c.Text)
		return types.TextPart{Text: text}, err
	case types.ToolResultPart:
		res, err := tokenizeResult(ctx, v, types.ToolResult{Parts: c.Parts})
		if err != nil {
			return nil, err
		}
		c.Parts = res.Parts
		return c, nil
	case types.ToolCallPart:
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
// arguments. Restorers are kept per part index, so interleaved parts never
// share one. Held text is flushed before its part ends, before the final
// delta, and when in closes. Thinking is never rewritten.
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
		texts := map[int]*StreamRestorer{}
		args := map[int]*StreamRestorer{}
		flush := func(i int) bool {
			if r, ok := texts[i]; ok {
				delete(texts, i)
				if rest := r.Flush(); rest != "" && !send(types.PartDelta{Index: i, Text: rest}) {
					return false
				}
			}
			if r, ok := args[i]; ok {
				delete(args, i)
				if rest := r.Flush(); rest != "" && !send(types.PartDelta{Index: i, Args: rest}) {
					return false
				}
			}
			return true
		}
		flushAll := func() bool {
			idx := make([]int, 0, len(texts)+len(args))
			for i := range texts {
				idx = append(idx, i)
			}
			for i := range args {
				if _, dup := texts[i]; !dup {
					idx = append(idx, i)
				}
			}
			sort.Ints(idx)
			for _, i := range idx {
				if !flush(i) {
					return false
				}
			}
			return true
		}
		for d := range in {
			ok := true
			switch x := d.(type) {
			case types.PartDelta:
				switch {
				case x.Text != "":
					r := texts[x.Index]
					if r == nil {
						r = NewStreamRestorer(v.Restore)
						texts[x.Index] = r
					}
					if s := r.Write(x.Text); s != "" {
						x.Text = s
						ok = send(x)
					}
				case x.Args != "":
					r := args[x.Index]
					if r == nil {
						r = NewStreamRestorer(restoreArgs)
						args[x.Index] = r
					}
					if s := r.Write(x.Args); s != "" {
						x.Args = s
						ok = send(x)
					}
				default:
					ok = send(x)
				}
			case types.PartEnd:
				ok = flush(x.Index)
				switch p := x.Part.(type) {
				case types.TextPart:
					p.Text = v.Restore(p.Text)
					x.Part = p
				case types.ToolCallPart:
					if p.Arguments != nil {
						p.Arguments = restoreValue(v, p.Arguments).(map[string]any)
						x.Part = p
					}
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
