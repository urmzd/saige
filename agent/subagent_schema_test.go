package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
)

func TestSubAgentResponseSchema(t *testing.T) {
	tests := []struct {
		name       string
		child      types.Provider
		wantOutput string
		wantErr    string
	}{
		{
			name:       "native child returns compact validated JSON",
			child:      &schemaProvider{schema: [][]types.Delta{agenttest.TextResponse("{\n  \"city\": \"Tokyo\"\n}")}},
			wantOutput: `{"city":"Tokyo"}`,
		},
		{
			name: "child without native support uses final_answer",
			child: &agenttest.ScriptedProvider{Responses: [][]types.Delta{
				agenttest.ToolCallResponse("f1", FinalAnswerToolName, map[string]any{"city": "Tokyo"}),
			}},
			wantOutput: `{"city":"Tokyo"}`,
		},
		{
			name:    "an answer that does not match fails the delegation",
			child:   &schemaProvider{schema: [][]types.Delta{agenttest.TextResponse(`{"town":"Tokyo"}`)}},
			wantErr: "missing required property",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent := NewAgent(AgentConfig{Provider: &agenttest.ScriptedProvider{}},
				WithSubAgents(SubAgentDef{Name: "geo", Provider: tt.child, ResponseSchema: cityPopulationSchema}))
			s, err := parent.InvokeSubAgent(context.Background(), "geo", "Where?")
			if err != nil {
				t.Fatal(err)
			}
			for range s.Deltas() {
			}
			res, err := s.SubAgentResult()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if res.Output != tt.wantOutput {
				t.Fatalf("Output = %q, want %q", res.Output, tt.wantOutput)
			}
			got, err := DecodeOutput[city](res)
			if err != nil || got.City != "Tokyo" {
				t.Fatalf("DecodeOutput = %+v, %v", got, err)
			}
		})
	}
}

func TestSchemaResultPolicy(t *testing.T) {
	tests := []struct {
		name    string
		policy  SchemaResult
		result  SubAgentResult
		want    string
		wantErr bool
	}{
		{"failed child", SchemaResult{Schema: cityPopulationSchema}, SubAgentResult{Error: "boom"}, "", true},
		{"stop result missing from the trace", SchemaResult{Schema: cityPopulationSchema}, SubAgentResult{StopToolCallID: "x"}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.policy.Select(tt.result)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("Select = %q, %v", got, err)
			}
		})
	}
	if _, err := DecodeOutput[city](SubAgentResult{Error: "boom"}); err == nil {
		t.Fatal("DecodeOutput accepted a failed result")
	}
	if _, err := DecodeOutput[city](SubAgentResult{Output: "not json"}); err == nil {
		t.Fatal("DecodeOutput accepted text")
	}
}

func TestSubAgentFailureOutcome(t *testing.T) {
	tests := []struct {
		name       string
		childErr   error
		policy     types.OutcomePolicy
		wantModels []string
		wantRoute  bool
	}{
		{
			name:       "failure escalates the parent",
			childErr:   errors.New("child broke"),
			policy:     types.EscalationLadder{Models: []string{"small", "large"}, Kinds: []types.OutcomeKind{types.OutcomeSubagentFailed}},
			wantModels: []string{"small", "large"},
			wantRoute:  true,
		},
		{
			name:       "policy that keeps the model",
			childErr:   errors.New("child broke"),
			policy:     types.EscalationLadder{Models: []string{"small"}},
			wantModels: []string{"small", "small"},
		},
		{
			name:       "success is not reported",
			policy:     types.OutcomePolicyFunc(func(context.Context, types.Outcome) (*types.Switch, error) { panic("observed a success") }),
			wantModels: []string{"small", "small"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parentProvider := newRecordingSwitcher("small", [][]types.Delta{
				agenttest.ToolCallResponse("c1", "delegate_to_child", map[string]any{"task": "t"}),
				agenttest.TextResponse("done"),
			})
			childResponses := [][]types.Delta{agenttest.TextResponse("ok")}
			var childErrs []error
			if tt.childErr != nil {
				childErrs = []error{tt.childErr}
			}
			child := SubAgentDef{Name: "child", Provider: &agenttest.ScriptedProvider{Responses: childResponses, Errors: childErrs}}
			a := NewAgent(AgentConfig{Provider: parentProvider}, WithSubAgents(child), WithOutcomePolicy(tt.policy))
			var routes []types.RouteDelta
			_, err := Collect(a.Invoke(context.Background(), []types.Message{types.NewUserMessage("go")}), func(d types.Delta) {
				if r, ok := d.(types.RouteDelta); ok {
					routes = append(routes, r)
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(*parentProvider.models, ",") != strings.Join(tt.wantModels, ",") {
				t.Fatalf("models = %v, want %v", *parentProvider.models, tt.wantModels)
			}
			if (len(routes) == 1) != tt.wantRoute {
				t.Fatalf("routes = %+v", routes)
			}
			// The switch never separates a tool call from its result.
			msgs, _ := a.Tree().FlattenBranch(a.Tree().Active())
			for i, m := range msgs {
				if am, ok := m.(types.AssistantMessage); ok && len(assistantToolCalls(&am)) > 0 {
					if len(toolResultsOf(msgs[i+1])) == 0 {
						t.Fatalf("message after the tool call is %T, want its results", msgs[i+1])
					}
				}
			}
		})
	}
}

// recordingSwitcher is a ScriptedProvider that can change models and
// records the model each call used.
type recordingSwitcher struct {
	*agenttest.ScriptedProvider
	model  string
	models *[]string
}

func newRecordingSwitcher(model string, responses [][]types.Delta) *recordingSwitcher {
	return &recordingSwitcher{ScriptedProvider: &agenttest.ScriptedProvider{Responses: responses}, model: model, models: &[]string{}}
}

func (p *recordingSwitcher) Name() string  { return "recording" }
func (p *recordingSwitcher) Model() string { return p.model }
func (p *recordingSwitcher) WithModel(m string) types.Provider {
	c := *p
	c.model = m
	return &c
}

func (p *recordingSwitcher) ChatStream(ctx context.Context, msgs []types.Message, tools []types.ToolDef) (<-chan types.Delta, error) {
	*p.models = append(*p.models, p.model)
	return p.ScriptedProvider.ChatStream(ctx, msgs, tools)
}
