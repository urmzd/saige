// Package otel provides OpenTelemetry tracing integration for SAIGE agents.
// Import this package only if you want tracing — it is fully opt-in.
package otel

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/urmzd/saige/agent/types"
)

// TracedProvider wraps a Provider and emits OTel spans for ChatStream calls.
type TracedProvider struct {
	Inner  types.Provider
	tracer trace.Tracer
}

// NewTracedProvider wraps a provider with tracing.
func NewTracedProvider(inner types.Provider, tracer trace.Tracer) *TracedProvider {
	return &TracedProvider{Inner: inner, tracer: tracer}
}

// Name delegates to the inner provider.
func (p *TracedProvider) Name() string {
	return types.ProviderName(p.Inner)
}

// ChatStream starts a span around the provider call and wraps the delta channel.
func (p *TracedProvider) ChatStream(ctx context.Context, messages []types.Message, tools []types.ToolDef) (<-chan types.Delta, error) {
	ctx, span := p.tracer.Start(ctx, "llm.chat_stream",
		trace.WithAttributes(
			attribute.String("gen_ai.system", p.Name()),
			attribute.Int("gen_ai.request.message_count", len(messages)),
			attribute.Int("gen_ai.request.tool_count", len(tools)),
		),
	)

	ch, err := p.Inner.ChatStream(ctx, messages, tools)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return nil, err
	}

	return wrapDeltaChannel(ch, span), nil
}

// ChatStreamWithSchema delegates structured output calls with tracing.
func (p *TracedProvider) ChatStreamWithSchema(ctx context.Context, messages []types.Message, tools []types.ToolDef, schema *types.ParameterSchema) (<-chan types.Delta, error) {
	sop, ok := p.Inner.(types.StructuredOutputProvider)
	if !ok {
		return p.ChatStream(ctx, messages, tools)
	}

	ctx, span := p.tracer.Start(ctx, "llm.chat_stream_with_schema",
		trace.WithAttributes(
			attribute.String("gen_ai.system", p.Name()),
			attribute.Int("gen_ai.request.message_count", len(messages)),
			attribute.Int("gen_ai.request.tool_count", len(tools)),
		),
	)

	ch, err := sop.ChatStreamWithSchema(ctx, messages, tools, schema)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return nil, err
	}

	return wrapDeltaChannel(ch, span), nil
}

// ContentSupport delegates to the inner provider if it implements ContentNegotiator.
func (p *TracedProvider) ContentSupport() types.ContentSupport {
	if cn, ok := p.Inner.(types.ContentNegotiator); ok {
		return cn.ContentSupport()
	}
	return types.ContentSupport{}
}

// wrapDeltaChannel reads from the inner channel, records usage events,
// and ends the span when the channel closes or an error arrives.
func wrapDeltaChannel(in <-chan types.Delta, span trace.Span) <-chan types.Delta {
	out := make(chan types.Delta, cap(in))
	go func() {
		defer close(out)
		defer span.End()

		start := time.Now()
		for d := range in {
			switch v := d.(type) {
			case types.UsageDelta:
				span.SetAttributes(
					attribute.Int("gen_ai.usage.input_tokens", v.PromptTokens),
					attribute.Int("gen_ai.usage.output_tokens", v.CompletionTokens),
				)
			case types.ErrorDelta:
				span.RecordError(v.Error)
				span.SetStatus(codes.Error, v.Error.Error())
			case types.DoneDelta:
				span.SetAttributes(
					attribute.Int64("gen_ai.response.duration_ms", time.Since(start).Milliseconds()),
				)
			}
			out <- d
		}
	}()
	return out
}
