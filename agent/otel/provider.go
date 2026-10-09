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

	"github.com/urmzd/saige/agent/provider/wrapper"
	"github.com/urmzd/saige/agent/types"
)

// TracedProvider wraps a Provider and emits OTel spans for ChatStream calls.
type TracedProvider struct {
	Inner  types.Provider
	tracer trace.Tracer
	opts   providerOptions
}

// ProviderOption configures a TracedProvider.
type ProviderOption func(*providerOptions)

type providerOptions struct {
	metrics  *Metrics
	redactor func(string) string
}

// WithProviderMetrics records the time to first chunk of every traced call on
// m. Without it the value is only set as a span attribute.
func WithProviderMetrics(m *Metrics) ProviderOption {
	return func(o *providerOptions) { o.metrics = m }
}

// WithProviderRedactor scrubs error messages before they are written to a
// span. See Config.Redactor.
func WithProviderRedactor(redact func(string) string) ProviderOption {
	return func(o *providerOptions) { o.redactor = redact }
}

// NewTracedProvider wraps a provider with tracing.
func NewTracedProvider(inner types.Provider, tracer trace.Tracer, opts ...ProviderOption) *TracedProvider {
	p := &TracedProvider{Inner: inner, tracer: tracer}
	for _, opt := range opts {
		opt(&p.opts)
	}
	return p
}

// Unwrap returns the inner provider, so optional interfaces behind the tracing
// layer stay discoverable through the provider wrapper chain.
func (p *TracedProvider) Unwrap() types.Provider { return p.Inner }

// Close implements types.Closer by closing the inner provider.
func (p *TracedProvider) Close() error { return types.CloseProvider(p.Inner) }

// Name delegates to the inner provider.
func (p *TracedProvider) Name() string {
	return types.ProviderName(p.Inner)
}

// Model delegates to the inner provider.
func (p *TracedProvider) Model() string {
	return types.ProviderModel(p.Inner)
}

// vendor names the provider for gen_ai.provider.name and metrics: the
// adapter beneath any decorators ("openai", not "retry(openai)"). A router
// or split names itself here; the serving route replaces it when the
// stream reports one.
func (p *TracedProvider) vendor() string {
	return wrapper.InnermostName(p.Inner)
}

// spanName builds the OTel GenAI span name: "{operation} {model}".
func spanName(operation, model string) string {
	if model == "" {
		return operation
	}
	return fmt.Sprintf("%s %s", operation, model)
}

// startChat opens a chat span with the request attributes shared by both
// streaming entry points.
func (p *TracedProvider) startChat(ctx context.Context, extra ...attribute.KeyValue) (context.Context, trace.Span) {
	model := types.ProviderModel(p.Inner)
	attrs := append([]attribute.KeyValue{
		attribute.String("gen_ai.operation.name", "chat"),
		attribute.String("gen_ai.provider.name", p.vendor()),
	}, extra...)
	if model != "" {
		attrs = append(attrs, attribute.String("gen_ai.request.model", model))
	}
	return p.tracer.Start(ctx, spanName("chat", model),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...),
	)
}

// ChatStream starts a span around the provider call and wraps the delta channel.
func (p *TracedProvider) ChatStream(ctx context.Context, messages []types.Message, tools []types.ToolDef) (<-chan types.Delta, error) {
	// The clock starts before the inner call so connection setup and response
	// headers count toward the time to first chunk.
	start := time.Now()
	ctx, span := p.startChat(ctx)

	ch, err := p.Inner.ChatStream(ctx, messages, tools)
	if err != nil {
		recordSpanError(span, err, p.opts.redactor)
		span.End()
		return nil, err
	}

	return p.wrapDeltaChannel(ctx, ch, span, start), nil
}

// ChatStreamWithSchema delegates structured output calls with tracing. When
// the inner provider cannot enforce a schema, a call with one fails with an
// error matching types.ErrSchemaUnsupported (and so ErrInvalidModelConfig)
// instead of dropping it; a fallback chain then tries its next member.
func (p *TracedProvider) ChatStreamWithSchema(ctx context.Context, messages []types.Message, tools []types.ToolDef, schema *types.ParameterSchema) (<-chan types.Delta, error) {
	sop, ok := p.Inner.(types.StructuredOutputProvider)
	if !ok {
		if schema != nil {
			return nil, p.unsupported(types.ErrSchemaUnsupported)
		}
		return p.ChatStream(ctx, messages, tools)
	}

	start := time.Now()
	ctx, span := p.startChat(ctx, attribute.String("gen_ai.output.type", "json"))

	ch, err := sop.ChatStreamWithSchema(ctx, messages, tools, schema)
	if err != nil {
		recordSpanError(span, err, p.opts.redactor)
		span.End()
		return nil, err
	}

	return p.wrapDeltaChannel(ctx, ch, span, start), nil
}

// ChatStreamWithOptions implements types.OptionsProvider, so per-request
// controls such as a tool choice reach the inner provider through the tracing
// layer. The controls that GenAI conventions name are recorded as request
// attributes. When the inner provider cannot receive options the call fails
// with an error matching types.ErrInvalidModelConfig rather than dropping
// them.
func (p *TracedProvider) ChatStreamWithOptions(ctx context.Context, messages []types.Message, tools []types.ToolDef, opts types.RequestOptions) (<-chan types.Delta, error) {
	start := time.Now()
	ctx, span := p.startChat(ctx, requestAttributes(opts)...)

	op, ok := p.Inner.(types.OptionsProvider)
	if !ok {
		err := p.unsupported(types.ErrOptionsUnsupported)
		recordSpanError(span, err, p.opts.redactor)
		span.End()
		return nil, err
	}

	ch, err := op.ChatStreamWithOptions(ctx, messages, tools, opts)
	if err != nil {
		recordSpanError(span, err, p.opts.redactor)
		span.End()
		return nil, err
	}

	return p.wrapDeltaChannel(ctx, ch, span, start), nil
}

// unsupported builds the permanent error for a request the inner provider
// cannot serve. It never reached the network.
func (p *TracedProvider) unsupported(sentinel error) error {
	return &types.ProviderError{
		Provider: p.Name(),
		Model:    p.Model(),
		Kind:     types.ErrorKindPermanent,
		Err:      fmt.Errorf("%w (provider %q)", sentinel, p.Name()),
	}
}

// requestAttributes renders the request options GenAI conventions define.
// Unset options are omitted.
func requestAttributes(o types.RequestOptions) []attribute.KeyValue {
	var attrs []attribute.KeyValue
	float := func(key string, v *float64) {
		if v != nil {
			attrs = append(attrs, attribute.Float64(key, *v))
		}
	}
	integer := func(key string, v *int64) {
		if v != nil {
			attrs = append(attrs, attribute.Int64(key, *v))
		}
	}
	float("gen_ai.request.temperature", o.Temperature)
	float("gen_ai.request.top_p", o.TopP)
	float("gen_ai.request.top_k", o.TopK)
	float("gen_ai.request.frequency_penalty", o.FrequencyPenalty)
	float("gen_ai.request.presence_penalty", o.PresencePenalty)
	integer("gen_ai.request.seed", o.Seed)
	integer("gen_ai.request.max_tokens", o.MaxOutputTokens)
	if len(o.StopSequences) > 0 {
		attrs = append(attrs, attribute.StringSlice("gen_ai.request.stop_sequences", o.StopSequences))
	}
	if o.ToolChoice != nil && o.ToolChoice.Mode != "" {
		attrs = append(attrs, attribute.String("saige.request.tool_choice", string(o.ToolChoice.Mode)))
		if o.ToolChoice.Name != "" {
			attrs = append(attrs, attribute.String("saige.request.tool_choice.name", o.ToolChoice.Name))
		}
	}
	return attrs
}

// WithModel implements types.ModelSwitcher: it re-targets the inner provider
// and keeps tracing attached. Without it a ConfigContent model switch was
// dropped whenever tracing was enabled, so traced runs silently ignored the
// requested model.
func (p *TracedProvider) WithModel(model string) types.Provider {
	return &TracedProvider{Inner: types.ProviderWithModel(p.Inner, model), tracer: p.tracer, opts: p.opts}
}

// ContentSupport delegates to the inner provider, preferring its model-level
// capability declaration over the adapter-level negotiator.
func (p *TracedProvider) ContentSupport() types.ContentSupport {
	return types.ProviderContentSupport(p.Inner)
}

// Capabilities implements types.CapabilityReporter by delegating to the inner
// provider. Tracing changes observability, not what the model accepts.
//
// TracedProvider always implements types.OptionsProvider, but it can only
// forward options the inner provider accepts. When the inner provider takes
// no options, the controls that travel only as options are removed, so the
// agent loop falls back to what it does for the unwrapped provider (for
// example withholding tools for ToolChoiceNone) instead of sending options
// that ChatStreamWithOptions must reject.
func (p *TracedProvider) Capabilities() types.ModelCapabilities {
	caps, _ := types.ProviderCapabilities(p.Inner)
	if _, ok := p.Inner.(types.OptionsProvider); !ok {
		caps = caps.Without(types.CapToolChoice, types.CapParallelToolControl)
	}
	return caps
}

// EffectiveOptions implements types.OptionsReporter by forwarding to the inner
// provider.
func (p *TracedProvider) EffectiveOptions() types.RequestOptions {
	o, _ := types.ProviderEffectiveOptions(p.Inner)
	return o
}

// NewSession returns a traced session of the inner provider.
func (p *TracedProvider) NewSession() types.Provider {
	return &TracedProvider{Inner: types.NewProviderSession(p.Inner), tracer: p.tracer, opts: p.opts}
}

// wrapDeltaChannel forwards the inner channel and ends the span when it
// closes.
//
// Usage is merged across deltas and written once at the end. Providers report
// usage in parts (prompt and cache counts first, completion counts last), and
// span attributes are last-write-wins, so setting them per delta would keep
// only the final part.
func (p *TracedProvider) wrapDeltaChannel(ctx context.Context, in <-chan types.Delta, span trace.Span, start time.Time) <-chan types.Delta {
	out := make(chan types.Delta, cap(in))
	go func() {
		defer close(out)

		var (
			usage     types.UsageDelta
			sawUsage  bool
			firstSeen bool
			route     types.RouteDelta
			sawRoute  bool
		)
		defer func() {
			if sawUsage {
				span.SetAttributes(usageAttributes(usage)...)
			}
			if sawRoute {
				span.SetAttributes(routeAttributes(route)...)
				// The span started with what the wrapped provider reports,
				// which for a router is "router" and a profile ID. The
				// serving route names the vendor and model actually used.
				if route.Provider != "" {
					span.SetAttributes(attribute.String("gen_ai.provider.name", route.Provider))
				}
				if route.Model != "" {
					span.SetAttributes(attribute.String("gen_ai.request.model", route.Model))
				}
				if route.Options != nil {
					// The serving attempt's effective options, not only the
					// per-call overrides recorded when the span started.
					span.SetAttributes(requestAttributes(*route.Options)...)
				}
			}
			span.End()
		}()

		for d := range in {
			switch v := d.(type) {
			case types.TextContentDelta:
				if !firstSeen {
					firstSeen = true
					ttft := time.Since(start)
					span.SetAttributes(attribute.Float64("gen_ai.response.time_to_first_chunk", ttft.Seconds()))
					if p.opts.metrics != nil {
						p.opts.metrics.RecordTimeToFirstChunk(ctx, "chat", p.vendor(), ttft)
					}
				}
			case types.UsageDelta:
				usage = usage.Merge(v)
				sawUsage = true
			case types.RouteDelta:
				// A router or split may emit one route per attempt; the last
				// one names the configuration that produced the response.
				route = v
				sawRoute = true
				attrs := routeAttributes(v)
				if v.Options != nil {
					attrs = append(attrs, requestAttributes(*v.Options)...)
				}
				span.AddEvent("saige.route.attempt", trace.WithAttributes(attrs...))
			case types.ErrorDelta:
				recordSpanError(span, v.Error, p.opts.redactor)
			}
			out <- d
		}
	}()
	return out
}

// usageAttributes renders merged usage as GenAI semantic convention attributes.
func usageAttributes(u types.UsageDelta) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.Int("gen_ai.usage.input_tokens", u.PromptTokens),
		attribute.Int("gen_ai.usage.output_tokens", u.CompletionTokens),
	}
	if u.CachedPromptTokens > 0 {
		attrs = append(attrs, attribute.Int("gen_ai.usage.cache_read.input_tokens", u.CachedPromptTokens))
	}
	if u.CacheWriteTokens > 0 {
		attrs = append(attrs, attribute.Int("gen_ai.usage.cache_creation.input_tokens", u.CacheWriteTokens))
	}
	if u.ResponseModel != "" {
		attrs = append(attrs, attribute.String("gen_ai.response.model", u.ResponseModel))
	}
	if u.ResponseID != "" {
		attrs = append(attrs, attribute.String("gen_ai.response.id", u.ResponseID))
	}
	if len(u.FinishReasons) > 0 {
		attrs = append(attrs, attribute.StringSlice("gen_ai.response.finish_reasons", u.FinishReasons))
	}
	if u.CacheHit {
		attrs = append(attrs, attribute.Bool("saige.response.cache_hit", true))
	}
	return attrs
}

// routeAttributes records which routing profile, provider, model and
// experiment arm served a call. Empty fields are omitted.
func routeAttributes(r types.RouteDelta) []attribute.KeyValue {
	var attrs []attribute.KeyValue
	add := func(key, value string) {
		if value != "" {
			attrs = append(attrs, attribute.String(key, value))
		}
	}
	add("saige.route.profile", r.Profile)
	add("saige.route.provider", r.Provider)
	add("saige.route.model", r.Model)
	add("saige.route.experiment", r.Experiment)
	add("saige.route.variant", r.Variant)
	add("saige.route.reason", r.Reason)
	add("saige.route.preset", r.Preset)
	add("saige.route.config_hash", r.ConfigHash)
	add("saige.catalog.revision", r.CatalogRevision)
	return attrs
}

// recordSpanError marks span as failed and attaches the error classification.
// The message is passed through redact, when set, before it reaches either the
// status description or the exception event.
func recordSpanError(span trace.Span, err error, redact func(string) string) {
	if err == nil {
		return
	}
	msg := err.Error()
	if redact != nil {
		msg = redact(msg)
		span.RecordError(redactedError{msg: msg})
	} else {
		span.RecordError(err)
	}
	span.SetStatus(codes.Error, msg)
	span.SetAttributes(errorAttributes(err)...)
}

// redactedError carries a scrubbed message to RecordError, so the exception
// event never sees the original text. The error's kind is still recorded
// separately through error.type.
type redactedError struct{ msg string }

func (e redactedError) Error() string { return e.msg }

// errorAttributes returns error.type plus the retry signals a classified error
// carries.
func errorAttributes(err error) []attribute.KeyValue {
	attrs := []attribute.KeyValue{attribute.String("error.type", errorType(err))}
	if !isClassified(err) {
		return attrs
	}
	attrs = append(attrs, attribute.Bool("saige.error.transient", types.IsTransient(err)))
	if d := types.RetryAfter(err); d > 0 {
		attrs = append(attrs, attribute.Int64("saige.error.retry_after_ms", d.Milliseconds()))
	}
	return attrs
}

// isClassified reports whether err carries an ErrorKind.
func isClassified(err error) bool {
	var pe *types.ProviderError
	var re *types.RemoteError
	var fe *types.FallbackError
	var te *types.ResponseTruncatedError
	return errors.As(err, &pe) || errors.As(err, &re) || errors.As(err, &fe) || errors.As(err, &te)
}

// errorType extracts a short error type string suitable for the error.type
// attribute. A classified error reports its kind ("rate_limit",
// "context_length", ...), so dashboards can group failures by cause rather
// than by Go type. A cancelled or timed-out context reports "canceled" or
// "timeout", a tool result flagged IsError reports "tool_error", and a
// recovered tool panic (an error with a ToolPanic() bool method that returns
// true) reports "panic". Anything else falls back to the Go type name.
func errorType(err error) string {
	var tre toolResultError
	var panicked interface{ ToolPanic() bool }
	switch {
	case errors.As(err, &panicked) && panicked.ToolPanic():
		return "panic"
	case errors.As(err, &tre):
		return "tool_error"
	case isClassified(err):
		return types.KindOf(err).String()
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	return fmt.Sprintf("%T", err)
}
