package otel

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/urmzd/saige/agent/types"
)

// TracedTool wraps a Tool and emits OTel spans for Execute calls.
type TracedTool struct {
	Inner  types.Tool
	tracer trace.Tracer
}

// NewTracedTool wraps a tool with tracing.
func NewTracedTool(inner types.Tool, tracer trace.Tracer) *TracedTool {
	return &TracedTool{Inner: inner, tracer: tracer}
}

// Definition delegates to the inner tool.
func (t *TracedTool) Definition() types.ToolDef {
	return t.Inner.Definition()
}

// Execute runs the tool within a traced span.
func (t *TracedTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	def := t.Inner.Definition()
	ctx, span := t.tracer.Start(ctx, "tool.execute",
		trace.WithAttributes(
			attribute.String("tool.name", def.Name),
		),
	)
	defer span.End()

	result, err := t.Inner.Execute(ctx, args)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return result, err
}

// WrapRegistry wraps all tools in a registry with tracing.
func WrapRegistry(reg *types.ToolRegistry, tracer trace.Tracer) *types.ToolRegistry {
	wrapped := types.NewToolRegistry()
	for _, def := range reg.Definitions() {
		tool, ok := reg.Get(def.Name)
		if ok {
			wrapped.Register(NewTracedTool(tool, tracer))
		}
	}
	return wrapped
}
