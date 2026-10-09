package otel

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/types"
)

// TracedTool wraps a Tool and emits OTel spans for Execute calls.
//
// It implements types.RichTool, so rich results (blocks, citations) pass
// through, and types.Cacheable, so a cache policy declared by the inner tool
// stays visible. Unwrap returns the inner tool. Use WrapTool rather than
// NewTracedTool when the inner tool may carry markers, a handoff, a sub-agent,
// or a Configurable binding: WrapTool keeps each of those working.
type TracedTool struct {
	Inner  types.Tool
	tracer trace.Tracer
	opts   toolOptions
}

// ToolOption configures a TracedTool.
type ToolOption func(*toolOptions)

type toolOptions struct {
	redactor func(string) string
}

// WithToolRedactor scrubs error messages before they are written to a span.
// See Config.Redactor.
func WithToolRedactor(redact func(string) string) ToolOption {
	return func(o *toolOptions) { o.redactor = redact }
}

var (
	_ types.RichTool     = (*TracedTool)(nil)
	_ types.Cacheable    = (*TracedTool)(nil)
	_ types.Configurable = (*tracedConfigurable)(nil)
)

// NewTracedTool wraps a tool with tracing. It wraps exactly what it is given;
// see WrapTool for the composition rules that keep approvals and handoffs
// working.
func NewTracedTool(inner types.Tool, tracer trace.Tracer, opts ...ToolOption) *TracedTool {
	t := &TracedTool{Inner: inner, tracer: tracer}
	for _, opt := range opts {
		opt(&t.opts)
	}
	return t
}

// Unwrap returns the inner tool.
func (t *TracedTool) Unwrap() types.Tool { return t.Inner }

// Definition delegates to the inner tool.
func (t *TracedTool) Definition() types.ToolDef {
	return t.Inner.Definition()
}

// CachePolicy implements types.Cacheable by reporting the inner tool's
// declaration. A tool with no declaration reports the zero, uncached policy,
// which is what it would report unwrapped.
func (t *TracedTool) CachePolicy() types.CachePolicy {
	return types.PolicyFor(t.Inner)
}

// Execute runs the tool within a traced span.
func (t *TracedTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	var result string
	err := t.traced(ctx, func(ctx context.Context) error {
		var err error
		result, err = t.Inner.Execute(ctx, args)
		return err
	})
	return result, err
}

// ExecuteRich implements types.RichTool. A rich inner tool's result passes
// through unchanged; a plain tool's text is wrapped in a text-only result,
// which the agent loop treats exactly like the plain Execute path.
func (t *TracedTool) ExecuteRich(ctx context.Context, args map[string]any) (types.ToolResult, error) {
	var (
		result types.ToolResult
		err    error
	)
	// The span error is reported through the span; the caller's error is err.
	_ = t.traced(ctx, func(ctx context.Context) error {
		if rt, ok := t.Inner.(types.RichTool); ok {
			result, err = rt.ExecuteRich(ctx, args)
		} else {
			result.Text, err = t.Inner.Execute(ctx, args)
		}
		if err == nil && result.IsError {
			// The agent loop treats a result flagged IsError as a failed
			// call, so the span reports it as one too. Only the span sees
			// this error; the caller gets the result unchanged.
			return toolResultError{msg: result.Text}
		}
		return err
	})
	return result, err
}

// toolResultError is the span error for a tool that signalled failure through
// ToolResult.IsError without returning a Go error.
type toolResultError struct{ msg string }

func (e toolResultError) Error() string { return e.msg }

// traced runs fn inside an execute_tool span. The error fn returns is recorded
// on the span and returned.
func (t *TracedTool) traced(ctx context.Context, fn func(context.Context) error) error {
	def := t.Inner.Definition()
	attrs := []attribute.KeyValue{
		attribute.String("gen_ai.operation.name", "execute_tool"),
		attribute.String("gen_ai.tool.name", def.Name),
		attribute.String("saige.tool.capability", string(def.Capability.Effective())),
	}
	if def.Description != "" {
		attrs = append(attrs, attribute.String("gen_ai.tool.description", def.Description))
	}
	ctx, span := t.tracer.Start(ctx, fmt.Sprintf("execute_tool %s", def.Name),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attrs...),
	)
	defer span.End()

	err := fn(ctx)
	recordSpanError(span, err, t.opts.redactor)
	return err
}

// tracedConfigurable is a TracedTool over a types.Configurable tool. It is a
// separate type so only tools that really are Configurable report the
// interface; binding returns a traced instance of the bound tool.
type tracedConfigurable struct {
	*TracedTool
	cfg types.Configurable
}

func (t *tracedConfigurable) ContextSchema() types.ParameterSchema { return t.cfg.ContextSchema() }
func (t *tracedConfigurable) Requires() []string                   { return t.cfg.Requires() }

// Configure binds the inner tool and traces the bound instance.
func (t *tracedConfigurable) Configure(tc types.ToolContext, deps types.Deps) (types.Tool, error) {
	bound, err := t.cfg.Configure(tc, deps)
	if err != nil {
		return nil, err
	}
	return wrapTool(bound, t.tracer, t.opts), nil
}

// WrapTool adds tracing to a tool without hiding what the agent loop looks for
// on it:
//
//   - A *types.MarkedTool is unwrapped, traced, and re-marked, so the
//     human-approval prompt still fires and the span covers only the work
//     that runs after approval.
//   - A tool implementing agent.HandoffSignaler or agent.SubAgentInvoker is
//     returned unchanged. The loop handles those itself, and a decorator in
//     front of one would turn a control transfer into a plain tool call.
//   - A types.Configurable tool stays Configurable, and binding it returns a
//     traced instance.
//   - A tool that is already traced is returned unchanged, so wrapping twice
//     does not nest spans.
func WrapTool(tool types.Tool, tracer trace.Tracer, opts ...ToolOption) types.Tool {
	var o toolOptions
	for _, opt := range opts {
		opt(&o)
	}
	return wrapTool(tool, tracer, o)
}

func wrapTool(tool types.Tool, tracer trace.Tracer, o toolOptions) types.Tool {
	switch v := tool.(type) {
	case nil:
		return nil
	case *TracedTool, *tracedConfigurable:
		return tool
	case *types.MarkedTool:
		return types.WithMarkers(wrapTool(v.Inner, tracer, o), v.Markers...)
	case agent.HandoffSignaler, agent.SubAgentInvoker:
		return tool
	case types.Configurable:
		return &tracedConfigurable{TracedTool: &TracedTool{Inner: tool, tracer: tracer, opts: o}, cfg: v}
	}
	return &TracedTool{Inner: tool, tracer: tracer, opts: o}
}

// WrapRegistry wraps all tools in a registry with tracing, following the
// composition rules of WrapTool. The source registry is left unchanged.
func WrapRegistry(reg *types.ToolRegistry, tracer trace.Tracer, opts ...ToolOption) *types.ToolRegistry {
	wrapped := types.NewToolRegistry()
	for _, tool := range reg.All() {
		wrapped.Register(WrapTool(tool, tracer, opts...))
	}
	return wrapped
}
