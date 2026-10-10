package types

import (
	"context"
	"strings"
)

// Request is one model call: the conversation as typed parts, the tools on
// offer, and the optional response schema and per-request controls.
type Request struct {
	Messages []Message
	Tools    []ToolDef
	// Schema constrains the answer to JSON matching it. Nil leaves the
	// answer free text. A provider that cannot enforce a schema rejects a
	// request that carries one (see StructuredOutputProvider).
	Schema *ParameterSchema
	// Options are per-request controls and dials applied over the
	// provider's configured ones. Nil sends none. A provider that cannot
	// receive them rejects a request that carries them (see
	// OptionsProvider).
	Options *RequestOptions
}

// Provider is the narrow LLM interface the agent loop needs. Stream sends
// one request and returns its deltas: part deltas for the model's output,
// then usage, then DoneDelta or an ErrorDelta.
//
// Model selection is handled via ConfigPart in the message tree, not as a
// parameter: providers that implement ModelSwitcher are re-targeted by the
// agent loop when a ConfigPart sets a model; others use their own
// configured default.
type Provider interface {
	Stream(ctx context.Context, req Request) (<-chan Delta, error)
}

// NamedProvider is an optional interface providers can implement
// for identification in logs and error messages.
type NamedProvider interface {
	Provider
	Name() string
}

// StructuredOutputProvider is an optional interface for providers whose
// Stream applies Request.Schema. A provider without it, or whose
// SupportsSchema reports false, cannot enforce a schema, and the agent loop
// rejects a request that needs one rather than drop it. A decorator reports
// true and rejects a schema its inner provider cannot take when the request
// arrives.
type StructuredOutputProvider interface {
	Provider
	SupportsSchema() bool
}

// AcceptsSchema reports whether p applies Request.Schema.
func AcceptsSchema(p Provider) bool {
	sp, ok := p.(StructuredOutputProvider)
	return ok && sp.SupportsSchema()
}

// ModelProvider is an optional interface providers can implement
// to expose the configured model name for telemetry and logging.
type ModelProvider interface {
	Provider
	Model() string
}

// ModelSwitcher is an optional interface providers can implement to produce
// a variant of themselves targeting a different model. The agent loop uses it
// to honor ConfigPart.Model at runtime.
type ModelSwitcher interface {
	Provider
	WithModel(model string) Provider
}

// Closer is an optional interface providers can implement for graceful shutdown.
type Closer interface {
	Close() error
}

// ProviderName returns the name of a provider if it implements NamedProvider,
// otherwise returns "unknown".
func ProviderName(p Provider) string {
	if np, ok := p.(NamedProvider); ok {
		return np.Name()
	}
	return "unknown"
}

// ProviderModel returns the model of a provider if it implements ModelProvider,
// otherwise returns an empty string.
func ProviderModel(p Provider) string {
	if mp, ok := p.(ModelProvider); ok {
		return mp.Model()
	}
	return ""
}

// ProviderWithModel returns a variant of p targeting the given model. It
// returns p unchanged when model is empty, already p's configured model, or
// p does not implement ModelSwitcher.
func ProviderWithModel(p Provider, model string) Provider {
	if model == "" || ProviderModel(p) == model {
		return p
	}
	if ms, ok := p.(ModelSwitcher); ok {
		return ms.WithModel(model)
	}
	return p
}

// CloseProvider closes a provider if it implements Closer, otherwise returns nil.
func CloseProvider(p Provider) error {
	if c, ok := p.(Closer); ok {
		return c.Close()
	}
	return nil
}

// GenerateText sends a single-turn user prompt with no tools and returns the
// concatenated text of the response. It is the plumbing behind each adapter's
// Generate method: the minimal seam used by eval judges, HyDE expansion,
// context compression, and KG extraction. The stream is always drained fully
// (so the producing goroutine never blocks); the first ErrorDelta wins.
func GenerateText(ctx context.Context, p Provider, prompt string) (string, error) {
	ch, err := p.Stream(ctx, Request{Messages: []Message{UserMsg(Text(prompt))}})
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	var genErr error
	for d := range ch {
		switch v := d.(type) {
		case PartDelta:
			sb.WriteString(v.Text)
		case ErrorDelta:
			if genErr == nil {
				genErr = v.Error
			}
		}
	}
	if genErr != nil {
		return "", genErr
	}
	return sb.String(), nil
}

// SessionProvider creates an independent provider routing session for a child.
// Immutable clients may be shared; sticky selection and in-flight state may not.
type SessionProvider interface {
	Provider
	NewSession() Provider
}

// NewProviderSession isolates optional routing state through provider decorators.
func NewProviderSession(provider Provider) Provider {
	if sessions, ok := provider.(SessionProvider); ok {
		return sessions.NewSession()
	}
	return provider
}
