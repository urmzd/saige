package otel

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/types"
)

// plainTool is a tool with no optional interfaces.
type plainTool struct {
	name string
	err  error
}

func (t plainTool) Definition() types.ToolDef { return types.ToolDef{Name: t.name} }
func (t plainTool) Execute(context.Context, map[string]any) (string, error) {
	return t.name + " ran", t.err
}

// richTool returns a citation alongside its text.
type richTool struct{ plainTool }

func (t richTool) ExecuteRich(context.Context, map[string]any) (types.ToolResult, error) {
	return types.ToolResult{Parts: []types.ToolOutputPart{types.Text("rich")}, Citations: []types.Citation{{URI: "https://example.com", Title: "src"}}}, nil
}

// isErrorTool reports failure through ToolResult.IsError without a Go error.
type isErrorTool struct{ plainTool }

func (t isErrorTool) ExecuteRich(context.Context, map[string]any) (types.ToolResult, error) {
	return types.ToolResult{Parts: []types.ToolOutputPart{types.Text(t.name + " failed")}, IsError: true}, nil
}

// cacheableTool declares a cache policy.
type cacheableTool struct{ plainTool }

func (cacheableTool) CachePolicy() types.CachePolicy {
	return types.CachePolicy{Enabled: true, TTL: time.Minute}
}

// configurableTool binds to a context and returns a new plain tool.
type configurableTool struct{ plainTool }

func (configurableTool) ContextSchema() types.ParameterSchema {
	return types.ParameterSchema{Type: "object"}
}
func (configurableTool) Requires() []string { return nil }
func (t configurableTool) Configure(types.ToolContext, types.Deps) (types.Tool, error) {
	return plainTool{name: t.name + "_bound"}, nil
}

// signalerTool transfers control like a handoff tool.
type signalerTool struct{ plainTool }

func (signalerTool) HandoffTarget() string { return "worker" }

// invokerTool delegates like a sub-agent tool.
type invokerTool struct{ plainTool }

func (invokerTool) InvokeAgent(context.Context, string) *agent.EventStream { return nil }

// unwrapTool walks a tool's Unwrap chain to its innermost tool.
func unwrapTool(t types.Tool) types.Tool {
	for i := 0; i < 16; i++ {
		u, ok := t.(interface{ Unwrap() types.Tool })
		if !ok {
			return t
		}
		t = u.Unwrap()
	}
	return t
}

// TestWrapToolKeepsOptionalInterfaces is the conformance table for the
// tracing decorator: every optional interface the agent loop looks for on a
// tool must still be found after WrapTool.
func TestWrapToolKeepsOptionalInterfaces(t *testing.T) {
	tracer, _ := newSpyTracer()
	marker := types.Marker{Kind: "human_approval", Message: "ok?"}

	tests := []struct {
		name         string
		tool         types.Tool
		unchanged    bool // returned as is
		marked       bool
		configurable bool
		cached       bool
	}{
		{name: "plain", tool: plainTool{name: "p"}},
		{name: "rich", tool: richTool{plainTool{name: "r"}}},
		{name: "cacheable", tool: cacheableTool{plainTool{name: "c"}}, cached: true},
		{name: "configurable", tool: configurableTool{plainTool{name: "cfg"}}, configurable: true},
		{name: "marked", tool: types.WithMarkers(plainTool{name: "m"}, marker), marked: true},
		{name: "marked rich", tool: types.WithMarkers(richTool{plainTool{name: "mr"}}, marker), marked: true},
		{name: "handoff signaler", tool: signalerTool{plainTool{name: "h"}}, unchanged: true},
		{name: "sub-agent invoker", tool: invokerTool{plainTool{name: "s"}}, unchanged: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := WrapTool(tt.tool, tracer)
			if got.Definition().Name != tt.tool.Definition().Name {
				t.Fatalf("definition name = %q, want %q", got.Definition().Name, tt.tool.Definition().Name)
			}
			if tt.unchanged {
				if got != tt.tool {
					t.Fatalf("WrapTool wrapped a control-transfer tool: %T", got)
				}
				return
			}

			if tt.marked {
				mt, ok := got.(*types.MarkedTool)
				if !ok {
					t.Fatalf("marked tool came back as %T; approval would be skipped", got)
				}
				if len(mt.Markers) != 1 || mt.Markers[0].Kind != marker.Kind {
					t.Fatalf("markers = %+v", mt.Markers)
				}
				if _, ok := mt.Inner.(*TracedTool); !ok {
					t.Fatalf("marked inner = %T, want *TracedTool", mt.Inner)
				}
				if unwrapTool(mt.Inner) != tt.tool.(*types.MarkedTool).Inner {
					t.Fatal("Unwrap does not reach the original inner tool")
				}
			} else if unwrapTool(got) != tt.tool {
				t.Fatal("Unwrap does not reach the original tool")
			}

			if _, ok := got.(types.RichTool); !ok {
				t.Error("traced tool is not a RichTool")
			}
			if p := types.PolicyFor(got); p.Enabled != tt.cached {
				t.Errorf("cache policy enabled = %v, want %v", p.Enabled, tt.cached)
			}
			c, isConfigurable := got.(types.Configurable)
			if isConfigurable != tt.configurable {
				t.Fatalf("Configurable = %v, want %v", isConfigurable, tt.configurable)
			}
			if isConfigurable {
				bound, err := c.Configure(types.ToolContext{}, types.Deps{})
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := bound.(*TracedTool); !ok {
					t.Fatalf("bound tool = %T, want *TracedTool", bound)
				}
				if bound.Definition().Name != "cfg_bound" {
					t.Fatalf("bound name = %q", bound.Definition().Name)
				}
			}

			if again := WrapTool(got, tracer); again != got {
				if mt, ok := again.(*types.MarkedTool); !ok || mt.Inner != got.(*types.MarkedTool).Inner {
					t.Error("wrapping twice nested a second tracing layer")
				}
			}
		})
	}
}

func TestTracedToolSpans(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name      string
		tool      types.Tool
		rich      bool
		wantErr   error
		wantText  string
		wantCites int
		// wantSpanErr is the error.type the span records. It defaults to the
		// Go type of wantErr when that is set.
		wantSpanErr string
	}{
		{name: "execute ok", tool: plainTool{name: "a"}, wantText: "a ran"},
		{name: "execute error", tool: plainTool{name: "a", err: boom}, wantErr: boom, wantText: "a ran"},
		{name: "rich passes citations", tool: richTool{plainTool{name: "r"}}, rich: true, wantText: "rich", wantCites: 1},
		{name: "rich over plain", tool: plainTool{name: "p"}, rich: true, wantText: "p ran"},
		{name: "rich over plain error", tool: plainTool{name: "p", err: boom}, rich: true, wantErr: boom, wantText: "p ran"},
		{name: "rich result flagged as error", tool: isErrorTool{plainTool{name: "e"}}, rich: true, wantText: "e failed", wantSpanErr: "tool_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracer, rec := newSpyTracer()
			tool := NewTracedTool(tt.tool, tracer)
			var (
				text  string
				cites int
				err   error
			)
			if tt.rich {
				var res types.ToolResult
				res, err = tool.ExecuteRich(context.Background(), nil)
				text, cites = res.Text(), len(res.Citations)
			} else {
				text, err = tool.Execute(context.Background(), nil)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if text != tt.wantText || cites != tt.wantCites {
				t.Fatalf("text = %q cites = %d, want %q %d", text, cites, tt.wantText, tt.wantCites)
			}

			spans := rec.all()
			if len(spans) != 1 {
				t.Fatalf("spans = %d, want 1", len(spans))
			}
			s := spans[0]
			if s.name != "execute_tool "+tt.tool.Definition().Name {
				t.Errorf("span name = %q", s.name)
			}
			if s.endCount() != 1 {
				t.Errorf("span ended %d times, want 1", s.endCount())
			}
			if got := s.str("saige.tool.capability"); got != "unknown" {
				t.Errorf("capability = %q, want unknown", got)
			}
			wantSpanErr := tt.wantSpanErr
			if wantSpanErr == "" && tt.wantErr != nil {
				wantSpanErr = "*errors.errorString"
			}
			if wantSpanErr != "" {
				if s.status != codes.Error || len(s.errs) != 1 {
					t.Errorf("status = %v errs = %d, want error status and one recorded error", s.status, len(s.errs))
				}
				if got := s.str("error.type"); got != wantSpanErr {
					t.Errorf("error.type = %q, want %q", got, wantSpanErr)
				}
			} else if s.status == codes.Error {
				t.Error("successful call marked as error")
			}
		})
	}
}

func TestWrapRegistryKeepsEveryTool(t *testing.T) {
	tracer, _ := newSpyTracer()
	src := types.NewToolRegistry(
		plainTool{name: "a"},
		types.WithMarkers(plainTool{name: "b"}, types.Marker{Kind: "human_approval"}),
	)
	got := WrapRegistry(src, tracer)
	if len(got.Definitions()) != 2 {
		t.Fatalf("definitions = %d, want 2", len(got.Definitions()))
	}
	b, _ := got.Get("b")
	if _, ok := b.(*types.MarkedTool); !ok {
		t.Fatalf("b = %T, want *types.MarkedTool", b)
	}
	a, _ := src.Get("a")
	if _, ok := a.(*TracedTool); ok {
		t.Fatal("source registry was modified")
	}
}
