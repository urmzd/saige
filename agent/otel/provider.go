// Package otel provides OpenTelemetry tracing integration for SAIGE agents.
// Import this package only if you want tracing: it is fully opt-in.
package otel

import (
	"context"
	"errors"
	"fmt"
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

// Model delegates to the inner provider.
func (p *TracedProvider) Model() string {
	return types.ProviderModel(p.Inner)
}

// spanName builds the OTel GenAI span name: "{operation} {model}".
func spanName(operation, model string) string {
	if model == "" {
		return operation
	}
	return fmt.Sprintf("%s %s", operation, model)
}

// ChatStream starts a span around the provider call and wraps the delta channel.
func (p *TracedProvider) ChatStream(ctx context.Context, messages []types.Message, tools []types.ToolDef) (<-chan types.Delta, error) {
	model := types.ProviderModel(p.Inner)
	ctx, span := p.tracer.Start(ctx, spanName("chat", model),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("gen_ai.operation.name", "chat"),
			attribute.String("gen_ai.provider.name", p.Name()),
		),
	)
	if model != "" {
		span.SetAttributes(attribute.String("gen_ai.request.model", model))
	}

	ch, err := p.Inner.ChatStream(ctx, messages, tools)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.SetAttributes(attribute.String("error.type", errorType(err)))
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

	model := types.ProviderModel(p.Inner)
	ctx, span := p.tracer.Start(ctx, spanName("chat", model),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("gen_ai.operation.name", "chat"),
			attribute.String("gen_ai.provider.name", p.Name()),
			attribute.String("gen_ai.output.type", "json"),
		),
	)
	if model != "" {
		span.SetAttributes(attribute.String("gen_ai.request.model", model))
	}

	ch, err := sop.ChatStreamWithSchema(ctx, messages, tools, schema)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.SetAttributes(attribute.String("error.type", errorType(err)))
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

		firstChunk := true
		start := time.Now()

		for d := range in {
			if firstChunk {
				_ = time.Since(start) // time-to-first-chunk available if needed
				firstChunk = false
			}

			switch v := d.(type) {
			case types.UsageDelta:
				span.SetAttributes(
					attribute.Int("gen_ai.usage.input_tokens", v.PromptTokens),
					attribute.Int("gen_ai.usage.output_tokens", v.CompletionTokens),
				)
				if v.ResponseModel != "" {
					span.SetAttributes(attribute.String("gen_ai.response.model", v.ResponseModel))
				}
				if v.ResponseID != "" {
					span.SetAttributes(attribute.String("gen_ai.response.id", v.ResponseID))
				}
				if len(v.FinishReasons) > 0 {
					span.SetAttributes(attribute.StringSlice("gen_ai.response.finish_reasons", v.FinishReasons))
				}
			case types.ErrorDelta:
				span.RecordError(v.Error)
				span.SetStatus(codes.Error, v.Error.Error())
				span.SetAttributes(attribute.String("error.type", errorType(v.Error)))
			case types.DoneDelta:
				// no-op; span ends on channel close
			}
			out <- d
		}
	}()
	return out
}

// errorType extracts a short error type string suitable for the error.type attribute.
func errorType(err error) string {
	var pe *types.ProviderError
	if errors.As(err, &pe) {
		switch pe.Kind {
		case types.ErrorKindTransient:
			return "transient"
		case types.ErrorKindPermanent:
			return "permanent"
		}
	}
	return fmt.Sprintf("%T", err)
}
