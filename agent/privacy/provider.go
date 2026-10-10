package privacy

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/urmzd/saige/agent/provider/wrapper"
	"github.com/urmzd/saige/agent/types"
)

// Provider is a decorator that tokenizes every request before it leaves the
// process and restores placeholders in the response. Use it when the host
// only needs to keep personal data away from the model vendor; the tree and
// telemetry then hold real values. Use ToolRedactor when they must not.
//
// Outgoing text, tool results, tool call arguments, refusals, citation
// quotes, audio transcripts and text-bearing media (a document or file of
// text, CSV or JSON with its bytes inline) are tokenized. Other media is
// governed by Media. Thinking blocks are left as they are in both
// directions: their signatures cover the text the model produced, and a
// changed block is rejected.
//
// The text a conversion below the decorator puts into the view (a
// transcript, a description, an extracted text) is tokenized too: the
// decorator sets a types.Egress boundary on the context of its calls, and
// every conversion decorator (convert.Provider) applies it.
//
// Streamed text, tool argument, refusal and transcript fragments are
// restored with a hold-back per part index, so interleaved parts never
// share one and a placeholder split across fragments is restored whole.
// Held text is released at the end of each part, on completion, and on
// error.
type Provider struct {
	wrapper.Base
	Config
}

// Config is a privacy decorator's vault and media policy.
type Config struct {
	// Vault tokenizes and restores personal data. Required.
	Vault Vault
	// Media is what happens to media the vault cannot tokenize. The
	// default is MediaRefuse with a Sensitive vault, else MediaPass.
	Media MediaPolicy
	// AllowAudioOut lets a model produce audio under a Sensitive vault.
	// The audio speaks the placeholders; only its transcript is restored.
	AllowAudioOut bool
}

// Option adjusts a Config before New validates it.
type Option func(*Config)

// WithMediaPolicy sets Config.Media.
func WithMediaPolicy(m MediaPolicy) Option { return func(c *Config) { c.Media = m } }

// WithAudioOut sets Config.AllowAudioOut.
func WithAudioOut() Option { return func(c *Config) { c.AllowAudioOut = true } }

// New wraps inner with cfg.Vault. A nil inner or vault is an error wrapping
// types.ErrInvalidConfig.
func New(inner types.Provider, cfg Config, opts ...Option) (*Provider, error) {
	for _, o := range opts {
		o(&cfg)
	}
	if inner == nil {
		return nil, fmt.Errorf("%w: privacy: no provider to wrap", types.ErrInvalidConfig)
	}
	if cfg.Vault == nil {
		return nil, fmt.Errorf("%w: privacy: Config.Vault is required", types.ErrInvalidConfig)
	}
	switch cfg.Media {
	case MediaDefault, MediaPass, MediaRefuse, MediaRequireText:
	default:
		return nil, fmt.Errorf("%w: privacy: unknown media policy %q", types.ErrInvalidConfig, cfg.Media)
	}
	return newProvider(inner, cfg), nil
}

func newProvider(inner types.Provider, cfg Config) *Provider {
	p := &Provider{Config: cfg}
	p.Base = wrapper.NewBase(inner, p.rewrap)
	return p
}

// rewrap keeps the vault and policy around another inner provider. The
// vault is shared, so placeholders keep their meaning after a model switch
// and in a new session.
func (p *Provider) rewrap(inner types.Provider) types.Provider { return newProvider(inner, p.Config) }

// MediaPolicy returns the policy in effect: Media, or the default for the
// vault.
func (p *Provider) MediaPolicy() MediaPolicy {
	if p.Media != MediaDefault {
		return p.Media
	}
	if sensitive(p.Vault) {
		return MediaRefuse
	}
	return MediaPass
}

// Name implements types.NamedProvider.
func (p *Provider) Name() string { return "privacy(" + types.NameOf(p.Inner) + ")" }

// Stream implements types.Provider. A schema or options the inner provider
// cannot receive are rejected, not dropped.
func (p *Provider) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	if req.Schema != nil && !types.AcceptsSchema(p.Inner) {
		return nil, p.unsupported("a response schema")
	}
	if req.Options != nil && !types.AcceptsOptions(p.Inner) {
		return nil, p.unsupported("request options")
	}
	return p.call(ctx, req.Messages, func(ctx context.Context, msgs []types.Message) (<-chan types.Delta, error) {
		r := req
		r.Messages = msgs
		return p.Inner.Stream(ctx, r)
	})
}

func (p *Provider) unsupported(what string) error {
	return &types.ProviderError{
		Provider: types.NameOf(p.Inner),
		Model:    types.ProviderModel(p.Inner),
		Kind:     types.ErrorKindPermanent,
		Err:      fmt.Errorf("%w: provider %q does not accept %s", types.ErrInvalidModelConfig, types.NameOf(p.Inner), what),
	}
}

func (p *Provider) call(ctx context.Context, messages []types.Message, send func(context.Context, []types.Message) (<-chan types.Delta, error)) (<-chan types.Delta, error) {
	msgs, err := TokenizeMessages(ctx, p.Vault, messages)
	if err != nil {
		// Fail closed: a request that could not be redacted is not sent.
		return nil, fmt.Errorf("privacy: tokenize request: %w", err)
	}
	eg := types.Egress{Vault: p.Vault, Opaque: opaque}
	switch p.MediaPolicy() {
	case MediaRefuse:
		if err := refuseMedia(msgs, "the media policy is refuse"); err != nil {
			return nil, err
		}
	case MediaRequireText:
		if !converts(p.Inner, 0) {
			// No conversion decorator below can replace the media with
			// text, so none may be sent.
			if err := refuseMedia(msgs, "the media policy is require_text and no conversion decorator serves the request"); err != nil {
				return nil, err
			}
		}
		eg.RequireText = true
	}
	in, err := send(types.WithEgress(ctx, eg), msgs)
	if err != nil {
		return nil, err
	}
	return restoreStream(ctx, p.Vault, in, sensitive(p.Vault) && !p.AllowAudioOut), nil
}

// TokenizeMessages returns a copy of messages with text, tool results, tool
// call arguments, refusals, citation quotes, audio transcripts and
// text-bearing media tokenized. Other media and thinking are left as they
// are. The input is not modified.
func TokenizeMessages(ctx context.Context, v Vault, messages []types.Message) ([]types.Message, error) {
	out := make([]types.Message, len(messages))
	for i, m := range messages {
		var err error
		switch msg := m.(type) {
		case types.SystemMessage:
			c := make([]types.SystemPart, len(msg.Parts))
			for j, part := range msg.Parts {
				var t types.Part
				if t, err = tokenizeContent(ctx, v, part); err != nil {
					return nil, err
				}
				c[j] = t.(types.SystemPart)
			}
			out[i] = types.SystemMessage{Parts: c}
		case types.UserMessage:
			c := make([]types.UserPart, len(msg.Parts))
			for j, part := range msg.Parts {
				var t types.Part
				if t, err = tokenizeContent(ctx, v, part); err != nil {
					return nil, err
				}
				c[j] = t.(types.UserPart)
			}
			out[i] = types.UserMessage{Parts: c}
		case types.AssistantMessage:
			c := make([]types.AssistantPart, len(msg.Parts))
			for j, part := range msg.Parts {
				var t types.Part
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

func tokenizeContent(ctx context.Context, v Vault, part types.Part) (types.Part, error) {
	switch c := part.(type) {
	case types.TextPart:
		text, err := v.Tokenize(ctx, c.Text)
		return types.TextPart{Text: text}, err
	case types.DocumentPart, types.FilePart:
		return tokenizeMedia(ctx, v, c)
	case types.RefusalPart:
		text, err := v.Tokenize(ctx, c.Text)
		c.Text = text
		return c, err
	case types.CitationPart:
		quote, err := v.Tokenize(ctx, c.Citation.Quote)
		c.Citation.Quote = quote
		return c, err
	case types.AudioOutPart:
		transcript, err := v.Tokenize(ctx, c.Transcript)
		c.Transcript = transcript
		return c, err
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

// field names a streamed payload a restorer holds text for.
type field uint8

const (
	fieldText field = iota
	fieldArgs
	fieldRefusal
	fieldTranscript
)

// slot is one part's payload: restorers are kept per part index and field,
// so interleaved parts never share held text.
type slot struct {
	index int
	field field
}

// payload returns the field d carries and its text. ok is false for a
// payload that is never rewritten: thinking, signatures and bytes.
func payload(d types.PartDelta) (field, string, bool) {
	switch {
	case d.Text != "":
		return fieldText, d.Text, true
	case d.Args != "":
		return fieldArgs, d.Args, true
	case d.Refusal != "":
		return fieldRefusal, d.Refusal, true
	case d.Transcript != "":
		return fieldTranscript, d.Transcript, true
	}
	return 0, "", false
}

// withPayload returns a delta for index carrying text in f.
func withPayload(index int, f field, text string) types.PartDelta {
	d := types.PartDelta{Index: index}
	switch f {
	case fieldText:
		d.Text = text
	case fieldArgs:
		d.Args = text
	case fieldRefusal:
		d.Refusal = text
	case fieldTranscript:
		d.Transcript = text
	}
	return d
}

// restorePart restores the placeholders in a finished part.
func restorePart(v Vault, part types.AssistantPart) types.AssistantPart {
	switch p := part.(type) {
	case types.TextPart:
		p.Text = v.Restore(p.Text)
		return p
	case types.ToolCallPart:
		if p.Arguments != nil {
			p.Arguments = restoreValue(v, p.Arguments).(map[string]any)
		}
		return p
	case types.RefusalPart:
		p.Text = v.Restore(p.Text)
		return p
	case types.CitationPart:
		p.Citation.Quote = v.Restore(p.Citation.Quote)
		return p
	case types.AudioOutPart:
		p.Transcript = v.Restore(p.Transcript)
		return p
	}
	return part
}

// restoreStream forwards in, restoring placeholders in text, tool call
// arguments, refusals and audio transcripts. Restorers are kept per part
// index and field, so interleaved parts never share one. Held text is
// flushed before its part ends, before the final delta, and when in closes.
// Thinking is never rewritten. With refuseAudio, a part of produced audio
// ends the stream with ErrAudioOutRefused.
//
//nolint:gocyclo // a single pass over a stream or loop state; splitting it would scatter shared state
func restoreStream(ctx context.Context, v Vault, in <-chan types.Delta, refuseAudio bool) <-chan types.Delta {
	out := make(chan types.Delta)
	go func() {
		defer close(out)
		drain := func() {
			go func() {
				for range in {
				}
			}()
		}
		send := func(d types.Delta) bool {
			select {
			case out <- d:
				return true
			case <-ctx.Done():
				drain()
				return false
			}
		}
		restoreArgs := func(s string) string { return s }
		if er, ok := v.(escapedRestorer); ok {
			restoreArgs = func(s string) string { return er.RestoreEscaped(s, jsonEscape) }
		}
		held := map[slot]*StreamRestorer{}
		restorer := func(k slot) *StreamRestorer {
			r := held[k]
			if r == nil {
				if k.field == fieldArgs {
					r = NewStreamRestorer(restoreArgs)
				} else {
					r = NewStreamRestorer(v.Restore)
				}
				held[k] = r
			}
			return r
		}
		flushSlots := func(keep func(slot) bool) bool {
			keys := make([]slot, 0, len(held))
			for k := range held {
				if keep(k) {
					keys = append(keys, k)
				}
			}
			sort.Slice(keys, func(i, j int) bool {
				if keys[i].index != keys[j].index {
					return keys[i].index < keys[j].index
				}
				return keys[i].field < keys[j].field
			})
			for _, k := range keys {
				r := held[k]
				delete(held, k)
				if rest := r.Flush(); rest != "" && !send(withPayload(k.index, k.field, rest)) {
					return false
				}
			}
			return true
		}
		flush := func(i int) bool { return flushSlots(func(k slot) bool { return k.index == i }) }
		flushAll := func() bool { return flushSlots(func(slot) bool { return true }) }
		for d := range in {
			ok := true
			switch x := d.(type) {
			case types.PartStart:
				if refuseAudio && x.Kind == types.KindAudioOut {
					if flushAll() {
						send(types.ErrorDelta{Error: ErrAudioOutRefused})
					}
					drain()
					return
				}
				ok = send(x)
			case types.PartDelta:
				f, text, rewrite := payload(x)
				if !rewrite {
					ok = send(x)
					break
				}
				if s := restorer(slot{x.Index, f}).Write(text); s != "" {
					ok = send(withPayload(x.Index, f, s))
				}
			case types.PartEnd:
				ok = flush(x.Index)
				if x.Part != nil {
					x.Part = restorePart(v, x.Part)
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
