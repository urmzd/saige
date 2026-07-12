package types

import (
	"context"
	"strings"
)

// Provider is the narrow LLM interface the agent loop needs.
// Model selection is handled via ConfigContent in the message tree,
// not as a parameter: providers that implement ModelSwitcher are
// re-targeted by the agent loop when a ConfigContent sets a model;
// others use their own configured default.
type Provider interface {
	ChatStream(ctx context.Context, messages []Message, tools []ToolDef) (<-chan Delta, error)
}

// NamedProvider is an optional interface providers can implement
// for identification in logs and error messages.
type NamedProvider interface {
	Provider
	Name() string
}

// StructuredOutputProvider is an optional interface for providers that support
// constraining LLM output to a JSON schema.
type StructuredOutputProvider interface {
	Provider
	ChatStreamWithSchema(ctx context.Context, messages []Message, tools []ToolDef, schema *ParameterSchema) (<-chan Delta, error)
}

// ModelProvider is an optional interface providers can implement
// to expose the configured model name for telemetry and logging.
type ModelProvider interface {
	Provider
	Model() string
}

// ModelSwitcher is an optional interface providers can implement to produce
// a variant of themselves targeting a different model. The agent loop uses it
// to honor ConfigContent.Model at runtime.
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
	ch, err := p.ChatStream(ctx, []Message{NewUserMessage(prompt)}, nil)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	var genErr error
	for d := range ch {
		switch v := d.(type) {
		case TextContentDelta:
			sb.WriteString(v.Content)
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
