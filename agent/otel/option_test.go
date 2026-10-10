package otel

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	mnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

// spyTracerProvider hands out one spyTracer.
type spyTracerProvider struct {
	noop.TracerProvider
	tracer spyTracer
}

func (p spyTracerProvider) Tracer(string, ...trace.TracerOption) trace.Tracer { return p.tracer }

func spyConfig() (Config, *spyRecorder) {
	tracer, rec := newSpyTracer()
	return Config{TracerProvider: spyTracerProvider{tracer: tracer}}, rec
}

// countingTool counts executions and can return a rich result.
type countingTool struct {
	name  string
	calls int
}

func (t *countingTool) Definition() types.ToolDef { return types.ToolDef{Name: t.name} }
func (t *countingTool) Execute(context.Context, map[string]any) (string, error) {
	t.calls++
	return "done", nil
}

type richCountingTool struct{ *countingTool }

func (t richCountingTool) ExecuteRich(ctx context.Context, args map[string]any) (types.ToolResult, error) {
	text, err := t.Execute(ctx, args)
	return types.ToolResult{Parts: []types.ToolOutputPart{types.Text(text)}, Citations: []types.Citation{{URI: "https://example.com/doc", Title: "doc"}}}, err
}

// TestWithTracingKeepsToolBehavior runs a real agent loop with tracing on and
// checks that approvals and rich results behave as they do without tracing.
func TestWithTracingKeepsToolBehavior(t *testing.T) {
	tests := []struct {
		name          string
		build         func(*countingTool) types.Tool
		wantMarker    bool
		wantCitations int
	}{
		{
			name:       "marked tool waits for approval",
			build:      func(c *countingTool) types.Tool { return types.WithMarkers(c, types.Marker{Kind: "human_approval"}) },
			wantMarker: true,
		},
		{
			name:          "rich tool keeps citations",
			build:         func(c *countingTool) types.Tool { return richCountingTool{c} },
			wantCitations: 1,
		},
		{
			name: "marked rich tool keeps both",
			build: func(c *countingTool) types.Tool {
				return types.WithMarkers(richCountingTool{c}, types.Marker{Kind: "human_approval"})
			},
			wantMarker:    true,
			wantCitations: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, rec := spyConfig()
			counter := &countingTool{name: "write"}
			prov := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
				agenttest.ToolCallResponse("c1", "write", map[string]any{}),
				agenttest.TextResponse("finished"),
			}}
			a := agent.NewAgent(agent.AgentConfig{
				Name:     "traced",
				Provider: prov,
				Tools:    types.NewToolRegistry(tt.build(counter)),
			}, WithTracing(cfg))

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("go"))})
			var markers, citations int
			for d := range stream.Deltas() {
				switch v := d.(type) {
				case types.MarkerDelta:
					markers++
					if counter.calls != 0 {
						t.Errorf("tool ran %d times before approval", counter.calls)
					}
					if err := stream.ResolveMarkerErr(v.ToolCallID, agent.Resolution{Approved: true}); err != nil {
						t.Errorf("resolve: %v", err)
					}
				case types.CitationDelta:
					citations++
				}
			}
			if err := stream.Wait(); err != nil {
				t.Fatal(err)
			}
			if (markers == 1) != tt.wantMarker {
				t.Errorf("markers = %d, want marker %v", markers, tt.wantMarker)
			}
			if citations != tt.wantCitations {
				t.Errorf("citations = %d, want %d", citations, tt.wantCitations)
			}
			if counter.calls != 1 {
				t.Errorf("tool calls = %d, want 1", counter.calls)
			}
			if spans := rec.named("execute_tool write"); len(spans) != 1 {
				t.Errorf("execute_tool spans = %d, want 1", len(spans))
			}
			if spans := rec.named("chat"); len(spans) != 2 {
				t.Errorf("chat spans = %d, want 2", len(spans))
			}
		})
	}
}

// recordingMetrics counts provider-call records.
type recordingMetrics struct {
	types.NoopMetrics
	providerCalls int
}

func (m *recordingMetrics) RecordProviderCall(context.Context, string, string, time.Duration, error) {
	m.providerCalls++
}

// failingMeter fails to create any histogram.
type failingMeter struct{ spyMeter }

func (failingMeter) Float64Histogram(string, ...metric.Float64HistogramOption) (metric.Float64Histogram, error) {
	return nil, errors.New("meter broken")
}

type failingMeterProvider struct{ mnoop.MeterProvider }

func (failingMeterProvider) Meter(string, ...metric.MeterOption) metric.Meter {
	return failingMeter{spyMeter{created: new([]string), log: new([]recordEvent)}}
}

func TestWithTracingMetrics(t *testing.T) {
	t.Run("existing sink keeps receiving records", func(t *testing.T) {
		user := &recordingMetrics{}
		cfg, _ := spyConfig()
		c := agent.AgentConfig{Metrics: user}
		WithTracing(cfg)(&c)
		if _, ok := c.Metrics.(fanoutMetrics); !ok {
			t.Fatalf("metrics = %T, want a fan-out over both sinks", c.Metrics)
		}
		c.Metrics.RecordProviderCall(context.Background(), "chat", "p", time.Second, nil)
		if user.providerCalls != 1 {
			t.Errorf("user sink received %d provider calls, want 1", user.providerCalls)
		}
	})

	t.Run("applying twice records once per call", func(t *testing.T) {
		user := &recordingMetrics{}
		cfg, _ := spyConfig()
		c := agent.AgentConfig{Metrics: user}
		WithTracing(cfg)(&c)
		WithTracing(cfg)(&c)
		f, ok := c.Metrics.(fanoutMetrics)
		if !ok {
			t.Fatalf("metrics = %T, want a fan-out", c.Metrics)
		}
		if len(f) != 2 {
			t.Errorf("fan-out members = %d, want 2 (user sink and one bridge)", len(f))
		}
		c.Metrics.RecordProviderCall(context.Background(), "chat", "p", time.Second, nil)
		if user.providerCalls != 1 {
			t.Errorf("user sink received %d provider calls, want 1", user.providerCalls)
		}
	})

	t.Run("no-op sink is replaced", func(t *testing.T) {
		cfg, _ := spyConfig()
		c := agent.AgentConfig{Metrics: types.NoopMetrics{}}
		WithTracing(cfg)(&c)
		if _, ok := c.Metrics.(*Metrics); !ok {
			t.Fatalf("metrics = %T, want *Metrics", c.Metrics)
		}
	})

	t.Run("failing meter is logged and leaves the sink", func(t *testing.T) {
		var buf bytes.Buffer
		user := &recordingMetrics{}
		cfg, _ := spyConfig()
		cfg.MeterProvider = failingMeterProvider{}
		c := agent.AgentConfig{Metrics: user, Logger: slog.New(slog.NewTextHandler(&buf, nil))}
		WithTracing(cfg)(&c)
		if c.Metrics != types.Metrics(user) {
			t.Errorf("metrics = %T, want the user sink unchanged", c.Metrics)
		}
		if !strings.Contains(buf.String(), "meter broken") {
			t.Errorf("log = %q, want the meter error", buf.String())
		}
	})
}

func TestWithTracingWrapsMembersOnce(t *testing.T) {
	cfg, _ := spyConfig()
	subs := []agent.SubAgentDef{{Name: "child", Provider: &fakeProvider{}, Tools: types.NewToolRegistry(plainTool{name: "t"})}, {Name: "inherit"}}
	c := agent.AgentConfig{
		Provider:  &fakeProvider{},
		SubAgents: subs,
		Handoffs:  []agent.HandoffDef{{Name: "worker", Provider: &fakeProvider{}}},
	}
	WithTracing(cfg)(&c)
	WithTracing(cfg)(&c)

	top, ok := c.Provider.(*TracedProvider)
	if !ok {
		t.Fatalf("provider = %T", c.Provider)
	}
	if _, nested := top.Inner.(*TracedProvider); nested {
		t.Error("applying the option twice nested tracing")
	}
	if _, ok := c.SubAgents[0].Provider.(*TracedProvider); !ok {
		t.Errorf("sub-agent provider = %T", c.SubAgents[0].Provider)
	}
	if tool, _ := c.SubAgents[0].Tools.Get("t"); tool == nil {
		t.Error("sub-agent tool lost")
	} else if _, ok := tool.(*TracedTool); !ok {
		t.Errorf("sub-agent tool = %T", tool)
	}
	if c.SubAgents[1].Provider != nil {
		t.Error("a sub-agent that inherits its parent's provider was given its own")
	}
	if _, ok := c.Handoffs[0].Provider.(*TracedProvider); !ok {
		t.Errorf("handoff provider = %T", c.Handoffs[0].Provider)
	}
	if _, ok := subs[0].Provider.(*TracedProvider); ok {
		t.Error("the caller's sub-agent slice was modified")
	}
}

func TestAgentTracerParentsRunSpans(t *testing.T) {
	cfg, rec := spyConfig()
	at := NewAgentTracer(cfg)
	tracer := cfg.tracer()

	ctx, end := at.StartAgent(context.Background(), "parent")
	ch, err := must.Get(NewTracedProvider(&fakeProvider{deltas: []types.Delta{types.DoneDelta{}}}, tracer)).Stream(ctx, types.Request{})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, ch)
	if _, err := NewTracedTool(plainTool{name: "t"}, tracer).Execute(ctx, nil); err != nil {
		t.Fatal(err)
	}
	childCtx, endChild := at.StartAgent(ctx, "child")
	if _, err := NewTracedTool(plainTool{name: "inner"}, tracer).Execute(childCtx, nil); err != nil {
		t.Fatal(err)
	}
	endChild(errors.New("child failed"))
	end(nil)
	end(errors.New("ignored"))

	root := rec.named("invoke_agent parent")
	child := rec.named("invoke_agent child")
	if len(root) != 1 || len(child) != 1 {
		t.Fatalf("agent spans: parent %d, child %d", len(root), len(child))
	}
	if root[0].parent != nil {
		t.Error("invoke_agent parent is not a root span")
	}
	for name, want := range map[string]*spySpan{
		"chat m1":            root[0],
		"execute_tool t":     root[0],
		"invoke_agent child": root[0],
		"execute_tool inner": child[0],
	} {
		spans := rec.named(name)
		if len(spans) != 1 || spans[0].parent != want {
			t.Errorf("%s is not a child of %s", name, want.name)
		}
	}
	if root[0].endCount() != 1 || root[0].status == codes.Error {
		t.Errorf("parent span ended %d times with status %v, want once and ok", root[0].endCount(), root[0].status)
	}
	if child[0].status != codes.Error {
		t.Error("failed child run not marked as error")
	}
	if got := root[0].str("gen_ai.agent.name"); got != "parent" {
		t.Errorf("agent name = %q", got)
	}
}

// capsOnlyProvider reports capabilities but cannot receive request options.
type capsOnlyProvider struct {
	inner *agenttest.ScriptedProvider
	caps  types.ModelCapabilities
}

func (p capsOnlyProvider) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	m, tools := req.Messages, req.Tools
	return p.inner.Stream(ctx, types.Request{Messages: m, Tools: tools})
}

func (p capsOnlyProvider) Capabilities() types.ModelCapabilities { return p.caps }

// capsOptionsProvider reports capabilities and accepts request options.
type capsOptionsProvider struct {
	*agenttest.ScriptedProvider
	caps types.ModelCapabilities
}

func (p capsOptionsProvider) Capabilities() types.ModelCapabilities { return p.caps }

func TestTracedProviderCapabilitiesFollowOptions(t *testing.T) {
	declared := types.ModelCapabilities{Caps: map[types.Capability]bool{
		types.CapToolChoice:          true,
		types.CapParallelToolControl: true,
		types.CapStreaming:           true,
	}}
	tests := []struct {
		name        string
		inner       types.Provider
		wantChoice  bool
		wantStreams bool
	}{
		{name: "inner without options", inner: capsOnlyProvider{caps: declared}, wantChoice: false, wantStreams: true},
		{name: "inner with options", inner: capsOptionsProvider{&agenttest.ScriptedProvider{}, declared}, wantChoice: true, wantStreams: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracer, _ := newSpyTracer()
			caps := must.Get(NewTracedProvider(tt.inner, tracer)).Capabilities()
			if got := caps.Supports(types.CapToolChoice); got != tt.wantChoice {
				t.Errorf("tool choice = %v, want %v", got, tt.wantChoice)
			}
			if got := caps.Supports(types.CapParallelToolControl); got != tt.wantChoice {
				t.Errorf("parallel tool control = %v, want %v", got, tt.wantChoice)
			}
			if got := caps.Supports(types.CapStreaming); got != tt.wantStreams {
				t.Errorf("streaming = %v, want %v", got, tt.wantStreams)
			}
		})
	}
	if !declared.Supports(types.CapToolChoice) {
		t.Error("the inner provider's capabilities were modified")
	}
}

// TestWithTracingToolChoiceNoneWithoutOptions runs a real agent whose provider
// declares tool choice but takes no request options. ToolChoiceNone must
// withhold the tools as it does without tracing, not fail the run.
func TestWithTracingToolChoiceNoneWithoutOptions(t *testing.T) {
	cfg, _ := spyConfig()
	scripted := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("plain answer")}}
	prov := capsOnlyProvider{inner: scripted, caps: types.ModelCapabilities{Caps: map[types.Capability]bool{
		types.CapToolChoice: true,
	}}}
	a := agent.NewAgent(agent.AgentConfig{
		Name:     "traced",
		Provider: prov,
		Tools:    types.NewToolRegistry(plainTool{name: "t"}),
	}, agent.WithToolChoice(types.ToolChoice{Mode: types.ToolChoiceNone}), WithTracing(cfg))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("go"))})
	for range stream.Deltas() {
	}
	if err := stream.Wait(); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if len(scripted.Calls) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(scripted.Calls))
	}
	call := scripted.Calls[0]
	if len(call.Tools) != 0 {
		t.Errorf("tools sent = %d, want none", len(call.Tools))
	}
	if call.Options != nil {
		t.Errorf("options sent = %+v, want none", call.Options)
	}
}
