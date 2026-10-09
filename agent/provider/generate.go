package provider

import (
	"context"

	"github.com/urmzd/saige/agent/provider/internal/generate"
	"github.com/urmzd/saige/agent/types"
)

// Generator is the single-prompt text seam used by evaluation judges, query
// expansion, context compression, and extraction. Every adapter implements
// it; AsGenerator provides it for any provider, including a decorator stack.
type Generator interface {
	Generate(ctx context.Context, prompt string) (string, error)
}

// GenerateResult is a single-turn response with its merged usage, so a caller
// can count the tokens and cost of an auxiliary call.
type GenerateResult struct {
	Text  string
	Usage types.UsageDelta
}

// GenerateWithUsage sends prompt as one user message with no tools. It keeps
// the merged usage, including finish reasons. A response cut short by the
// output limit returns an error matching types.ErrResponseTruncated, with the
// partial text and the usage still set on the result.
func GenerateWithUsage(ctx context.Context, p types.Provider, prompt string) (GenerateResult, error) {
	res, err := generate.Run(ctx, p, prompt)
	return GenerateResult{Text: res.Text, Usage: res.Usage}, err
}

// Generate is GenerateWithUsage reduced to the text. Unlike
// types.GenerateText it reports a truncated response as an error.
func Generate(ctx context.Context, p types.Provider, prompt string) (string, error) {
	return generate.Text(ctx, p, prompt)
}

// AsGenerator returns p as a Generator. The calls go through p itself, so
// retry, fallback, routing, caching, and tracing decorators apply to them.
func AsGenerator(p types.Provider) *ProviderGenerator {
	return &ProviderGenerator{Provider: p}
}

// ProviderGenerator adapts a provider to Generator.
type ProviderGenerator struct {
	Provider types.Provider
}

// Generate implements Generator.
func (g *ProviderGenerator) Generate(ctx context.Context, prompt string) (string, error) {
	return Generate(ctx, g.Provider, prompt)
}

// GenerateWithUsage returns the text with its usage. See GenerateWithUsage.
func (g *ProviderGenerator) GenerateWithUsage(ctx context.Context, prompt string) (GenerateResult, error) {
	return GenerateWithUsage(ctx, g.Provider, prompt)
}
