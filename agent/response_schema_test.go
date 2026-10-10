package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

var cityPopulationSchema = &types.ParameterSchema{
	Type:     "object",
	Required: []string{"city"},
	Properties: map[string]types.PropertyDef{
		"city": {Type: "string"},
	},
}

// schemaProvider scripts plain turns and schema-constrained turns
// separately, and records which kind each call was.
type schemaProvider struct {
	mu       sync.Mutex
	plain    [][]types.Delta
	schema   [][]types.Delta
	calls    []string
	lastMsgs []types.Message
}

func (p *schemaProvider) next(kind string, script *[][]types.Delta, msgs []types.Message) <-chan types.Delta {
	p.mu.Lock()
	p.calls = append(p.calls, kind)
	p.lastMsgs = msgs
	var deltas []types.Delta
	if len(*script) > 0 {
		deltas, *script = (*script)[0], (*script)[1:]
	}
	p.mu.Unlock()
	ch := make(chan types.Delta, len(deltas))
	for _, d := range deltas {
		ch <- d
	}
	close(ch)
	return ch
}

func (p *schemaProvider) chatStream(_ context.Context, msgs []types.Message, _ []types.ToolDef) (<-chan types.Delta, error) {
	return p.next("plain", &p.plain, msgs), nil
}

// Stream implements types.Provider.
func (p *schemaProvider) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	if req.Schema != nil {
		return p.chatStreamWithSchema(ctx, req.Messages, req.Tools, req.Schema)
	}
	return p.chatStream(ctx, req.Messages, req.Tools)
}

// SupportsSchema implements types.StructuredOutputProvider.
func (p *schemaProvider) SupportsSchema() bool { return true }

func (p *schemaProvider) chatStreamWithSchema(_ context.Context, msgs []types.Message, _ []types.ToolDef, _ *types.ParameterSchema) (<-chan types.Delta, error) {
	return p.next("schema", &p.schema, msgs), nil
}

// noSchemaModel declares a known model without structured output.
type noSchemaModel struct{ schemaProvider }

func (*noSchemaModel) Name() string  { return "test" }
func (*noSchemaModel) Model() string { return "plain-model" }
func (*noSchemaModel) Capabilities() types.ModelCapabilities {
	return types.ModelCapabilities{Known: true}
}

// withdrawnSchemaModel keeps a native structured-output mode but has dropped
// CapStructuredOutput, the shape an adapter reports when its current
// configuration (for example, thinking) cannot apply a schema.
type withdrawnSchemaModel struct{ schemaProvider }

func (*withdrawnSchemaModel) Name() string  { return "test" }
func (*withdrawnSchemaModel) Model() string { return "withdrawn-model" }
func (*withdrawnSchemaModel) Capabilities() types.ModelCapabilities {
	return types.ModelCapabilities{
		Known:            true,
		StructuredOutput: types.StructuredOutputNative,
		Caps:             map[types.Capability]bool{types.CapStreaming: true, types.CapTools: true},
	}
}

// claimsSchemaModel declares structured output but has no schema-aware call,
// so a schema sent to it would be dropped.
type claimsSchemaModel struct{ agenttest.ScriptedProvider }

func (*claimsSchemaModel) Name() string  { return "test" }
func (*claimsSchemaModel) Model() string { return "claims-model" }
func (*claimsSchemaModel) Capabilities() types.ModelCapabilities {
	return types.ModelCapabilities{Known: true, StructuredOutput: types.StructuredOutputNative}
}

func lastAssistantText(t *testing.T, a *Agent) string {
	t.Helper()
	msgs, err := a.Tree().FlattenBranch(a.Tree().Active())
	if err != nil {
		t.Fatal(err)
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if am, ok := msgs[i].(types.AssistantMessage); ok {
			var sb strings.Builder
			for _, c := range am.Parts {
				if tc, ok := c.(types.TextPart); ok {
					sb.WriteString(tc.Text)
				}
			}
			return sb.String()
		}
	}
	return ""
}

func TestResponseSchemaUnsupportedFailsFast(t *testing.T) {
	tests := []struct {
		name     string
		provider types.Provider
	}{
		{"provider without structured output", &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("x")}}},
		{"known model declaring no structured output", &noSchemaModel{schemaProvider{plain: [][]types.Delta{agenttest.TextResponse("x")}}}},
		{"declared structured output without a schema-aware call", &claimsSchemaModel{agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("x")}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := must.Get(New(Config{Provider: tt.provider}, WithResponseSchema(cityPopulationSchema)))
			s := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("go"))})
			for range s.Deltas() {
			}
			if err := s.Wait(); !errors.Is(err, types.ErrInvalidModelConfig) {
				t.Fatalf("Wait = %v, want ErrInvalidModelConfig", err)
			}
		})
	}
}

func TestResponseSchemaWithTools(t *testing.T) {
	lookup := &agenttest.MockTool{Def: types.ToolDef{Name: "lookup"}, Result: "Tokyo"}
	tests := []struct {
		name      string
		plain     [][]types.Delta
		schema    [][]types.Delta
		wantCalls []string
		wantFinal string
	}{
		{
			name: "free-text draft is replaced by a schema turn",
			plain: [][]types.Delta{
				agenttest.ToolCallResponse("c1", "lookup", nil),
				agenttest.TextResponse("The city is Tokyo."),
			},
			schema:    [][]types.Delta{agenttest.TextResponse(`{"city":"Tokyo"}`)},
			wantCalls: []string{"plain", "plain", "schema"},
			wantFinal: `{"city":"Tokyo"}`,
		},
		{
			name: "draft that already satisfies the schema is kept",
			plain: [][]types.Delta{
				agenttest.ToolCallResponse("c1", "lookup", nil),
				agenttest.TextResponse(`{"city":"Tokyo"}`),
			},
			wantCalls: []string{"plain", "plain"},
			wantFinal: `{"city":"Tokyo"}`,
		},
		{
			name: "JSON missing a required field is not accepted as final",
			plain: [][]types.Delta{
				agenttest.TextResponse(`{"town":"Tokyo"}`),
			},
			schema:    [][]types.Delta{agenttest.TextResponse(`{"city":"Tokyo"}`)},
			wantCalls: []string{"plain", "schema"},
			wantFinal: `{"city":"Tokyo"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &schemaProvider{plain: tt.plain, schema: tt.schema}
			a := must.Get(New(Config{Provider: p, Tools: types.NewToolRegistry(lookup)}, WithResponseSchema(cityPopulationSchema)))
			s := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("Where?"))})
			for range s.Deltas() {
			}
			if err := s.Wait(); err != nil {
				t.Fatal(err)
			}
			if strings.Join(p.calls, ",") != strings.Join(tt.wantCalls, ",") {
				t.Fatalf("calls = %v, want %v", p.calls, tt.wantCalls)
			}
			final := lastAssistantText(t, a)
			if final != tt.wantFinal {
				t.Fatalf("final answer = %q, want %q", final, tt.wantFinal)
			}
			var obj map[string]any
			if err := json.Unmarshal([]byte(final), &obj); err != nil {
				t.Fatalf("final answer is not JSON: %v", err)
			}
			// The discarded draft never enters the transcript, so the schema
			// request ends with the tool result or user turn, not an assistant.
			if len(tt.schema) > 0 {
				last := p.lastMsgs[len(p.lastMsgs)-1]
				if _, isAssistant := last.(types.AssistantMessage); isAssistant {
					t.Error("the schema request ended with the discarded draft")
				}
			}
		})
	}
}

// optionsSchemaProvider is a schemaProvider that accepts request options and
// declares tool choice and native structured output.
type optionsSchemaProvider struct {
	schemaProvider
	optionCalls int
}

func (p *optionsSchemaProvider) Capabilities() types.ModelCapabilities {
	return types.ModelCapabilities{Known: true, StructuredOutput: types.StructuredOutputNative}.
		With(types.CapStreaming, types.CapTools, types.CapToolChoice, types.CapStructuredOutput)
}

func (p *optionsSchemaProvider) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	if req.Options == nil {
		return p.schemaProvider.Stream(ctx, req)
	}
	p.mu.Lock()
	p.optionCalls++
	p.mu.Unlock()
	return p.schemaProvider.Stream(ctx, types.Request{Messages: req.Messages, Tools: req.Tools})
}

func (p *optionsSchemaProvider) SupportsOptions() bool { return true }

// TestToolChoiceNoneKeepsResponseSchema checks that a "none" tool choice on
// an agent with no tools does not route the call around the response schema.
func TestToolChoiceNoneKeepsResponseSchema(t *testing.T) {
	for _, mode := range []types.ToolChoiceMode{types.ToolChoiceNone, types.ToolChoiceAuto} {
		t.Run(string(mode), func(t *testing.T) {
			p := &optionsSchemaProvider{schemaProvider: schemaProvider{
				plain:  [][]types.Delta{agenttest.TextResponse("not json")},
				schema: [][]types.Delta{agenttest.TextResponse(`{"city":"Tokyo"}`)},
			}}
			a := must.Get(New(Config{Provider: p}, WithResponseSchema(cityPopulationSchema), WithToolChoice(types.ToolChoice{Mode: mode})))
			s := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("go"))})
			for range s.Deltas() {
			}
			if err := s.Wait(); err != nil {
				t.Fatal(err)
			}
			if len(p.calls) != 1 || p.calls[0] != "schema" || p.optionCalls != 0 {
				t.Fatalf("calls = %v, option calls = %d; want one schema call", p.calls, p.optionCalls)
			}
			if got := lastAssistantText(t, a); got != `{"city":"Tokyo"}` {
				t.Fatalf("final = %q", got)
			}
		})
	}
}
