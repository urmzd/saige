package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
)

// detachedRunner runs each step under a context that does not descend from
// the run's, as a durable engine does when it builds the step context from
// its own.
type detachedRunner struct{}

func (detachedRunner) RunStep(_ context.Context, _ string, fn func(context.Context) (types.StepResult, error)) (types.StepResult, error) {
	return fn(context.Background())
}

func TestStructuredNativeUnderDetachedStepRunner(t *testing.T) {
	answer := []types.Delta{
		types.TextStartDelta{},
		types.TextContentDelta{Content: `{"city": "Tok`},
		types.TextContentDelta{Content: `yo"}`},
		types.TextEndDelta{},
	}
	tests := []struct {
		name      string
		plain     [][]types.Delta
		withTools bool
		wantCalls []string
	}{
		{
			name:      "schema sent on the only turn",
			wantCalls: []string{"schema"},
		},
		{
			name:      "schema turn after a tool turn ends in prose",
			plain:     [][]types.Delta{agenttest.TextResponse("It is Tokyo.")},
			withTools: true,
			wantCalls: []string{"plain", "schema"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &schemaProvider{plain: tt.plain, schema: [][]types.Delta{answer}}
			cfg := AgentConfig{Provider: p, StepRunner: detachedRunner{}}
			if tt.withTools {
				cfg.Tools = types.NewToolRegistry(&types.ToolFunc{
					Def: types.ToolDef{Name: "lookup", Parameters: types.ParameterSchema{Type: "object"}},
					Fn:  func(context.Context, map[string]any) (string, error) { return "", nil },
				})
			}
			a := NewAgent(cfg)
			var partials int
			got, _, err := Structured(context.Background(), a, []types.Message{types.NewUserMessage("Where?")}, OutputSpec[city]{
				Mode: OutputNative,
				OnDelta: func(d types.Delta) {
					if _, ok := d.(types.PartialJSONDelta); ok {
						partials++
					}
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if got.City != "Tokyo" {
				t.Fatalf("got %+v", got)
			}
			if !slices.Equal(p.calls, tt.wantCalls) {
				t.Fatalf("calls = %v, want %v", p.calls, tt.wantCalls)
			}
			if partials == 0 {
				t.Fatal("no PartialJSONDelta was emitted")
			}
		})
	}
}

func TestConfiguredSchemaChecksTextAnswer(t *testing.T) {
	valid := agenttest.TextResponse("```json\n{\"city\":\"Tokyo\"}\n```")
	invalid := agenttest.TextResponse(`{"town":"Tokyo"}`)
	prose := agenttest.TextResponse("Tokyo, I think.")
	tests := []struct {
		name      string
		mode      OutputMode
		responses [][]types.Delta
		wantCalls int
		wantErr   bool
	}{
		{"tool mode accepts a matching text answer", OutputTool, [][]types.Delta{valid}, 1, false},
		{"tool mode repairs a mismatch", OutputTool, [][]types.Delta{invalid, valid}, 2, false},
		{"tool mode fails after the repairs", OutputTool, [][]types.Delta{invalid, prose, invalid}, 3, true},
		{"prompt mode accepts a fenced answer", OutputPrompt, [][]types.Delta{valid}, 1, false},
		{"prompt mode repairs prose", OutputPrompt, [][]types.Delta{prose, valid}, 2, false},
		{"prompt mode fails after the repairs", OutputPrompt, [][]types.Delta{prose, prose, invalid}, 3, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &agenttest.ScriptedProvider{Responses: tt.responses}
			a := NewAgent(AgentConfig{Provider: p}, WithResponseSchema(cityPopulationSchema), WithOutputMode(tt.mode))
			_, err := Collect(a.Invoke(context.Background(), []types.Message{types.NewUserMessage("Where?")}), nil)
			if tt.wantErr != errors.Is(err, ErrSchemaInvalid) {
				t.Fatalf("err = %v, want ErrSchemaInvalid: %v", err, tt.wantErr)
			}
			if !tt.wantErr && err != nil {
				t.Fatal(err)
			}
			reqs := p.Requests()
			if len(reqs) != tt.wantCalls {
				t.Fatalf("calls = %d, want %d", len(reqs), tt.wantCalls)
			}
			if tt.wantCalls > 1 && !strings.Contains(lastRequestText(reqs[1].Messages), "was not accepted") {
				t.Fatal("the repair request does not carry the error")
			}
		})
	}
}

func TestSubAgentOutputModeFollowsFinalProvider(t *testing.T) {
	tests := []struct {
		name    string
		options []AgentOption
		want    OutputMode
	}{
		{"native provider keeps the native path", nil, OutputAuto},
		{
			name:    "an option that swaps in a provider without native support",
			options: []AgentOption{func(c *AgentConfig) { c.Provider = &agenttest.ScriptedProvider{} }},
			want:    OutputTool,
		},
		{
			name: "an explicit mode from an option is kept",
			options: []AgentOption{func(c *AgentConfig) {
				c.Provider = &agenttest.ScriptedProvider{}
				c.OutputMode = OutputPrompt
			}},
			want: OutputPrompt,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			def := SubAgentDef{Name: "geo", Provider: &schemaProvider{}, ResponseSchema: cityPopulationSchema, Options: tt.options}
			child := NewAgent(inheritConfig(AgentConfig{}, def, nil), append(slices.Clone(def.Options), resolveChildOutputMode)...)
			if child.cfg.OutputMode != tt.want {
				t.Fatalf("OutputMode = %q, want %q", child.cfg.OutputMode, tt.want)
			}
		})
	}

	// End to end: the swapped-in provider answers through final_answer.
	swapped := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("f1", FinalAnswerToolName, map[string]any{"city": "Tokyo"}),
	}}
	parent := NewAgent(AgentConfig{Provider: &agenttest.ScriptedProvider{}},
		WithSubAgents(SubAgentDef{
			Name: "geo", Provider: &schemaProvider{}, ResponseSchema: cityPopulationSchema,
			Options: []AgentOption{func(c *AgentConfig) { c.Provider = swapped }},
		}))
	s, err := parent.InvokeSubAgent(context.Background(), "geo", "Where?")
	if err != nil {
		t.Fatal(err)
	}
	for range s.Deltas() {
	}
	res, err := s.SubAgentResult()
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != `{"city":"Tokyo"}` {
		t.Fatalf("Output = %q", res.Output)
	}
}
