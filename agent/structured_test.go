package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

type city struct {
	City string `json:"city"`
}

// switchingProvider is a schemaProvider that can change models and records
// the model each call used.
type switchingProvider struct {
	*schemaProvider
	model  string
	mu     *sync.Mutex
	models *[]string
}

func newSwitchingProvider(model string, schema [][]types.Delta) *switchingProvider {
	return &switchingProvider{schemaProvider: &schemaProvider{schema: schema}, model: model, mu: &sync.Mutex{}, models: &[]string{}}
}

func (p *switchingProvider) Name() string  { return "switching" }
func (p *switchingProvider) Model() string { return p.model }
func (p *switchingProvider) WithTarget(t types.Target) (types.Provider, error) {
	m := string(t.Model)
	c := *p
	c.model = m
	return &c, nil
}

func (p *switchingProvider) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	if req.Schema != nil {
		p.mu.Lock()
		*p.models = append(*p.models, p.model)
		p.mu.Unlock()
	}
	return p.schemaProvider.Stream(ctx, req)
}

func lastRequestText(msgs []types.Message) string {
	var sb strings.Builder
	for _, m := range msgs {
		switch v := m.(type) {
		case types.SystemMessage:
			for _, c := range v.Parts {
				if tc, ok := c.(types.TextPart); ok {
					sb.WriteString(tc.Text)
				}
			}
		case types.UserMessage:
			for _, c := range v.Parts {
				if tc, ok := c.(types.TextPart); ok {
					sb.WriteString(tc.Text)
				}
			}
		}
	}
	return sb.String()
}

func TestStructuredNative(t *testing.T) {
	p := &schemaProvider{schema: [][]types.Delta{{
		types.PartStart{Index: 0, Kind: types.KindText},
		types.PartDelta{Index: 0, Text: `{"city": "Tok`},
		types.PartDelta{Index: 0, Text: `yo"}`},
		types.PartEnd{Index: 0},
	}}}
	a := must.Get(New(Config{Provider: p}))
	var partials []string
	got, res, err := Structured(context.Background(), a, []types.Message{types.UserMsg(types.Text("Where?"))}, OutputSpec[city]{
		OnDelta: func(d types.Delta) {
			if pj, ok := d.(types.PartialJSONDelta); ok {
				partials = append(partials, string(pj.JSON))
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.City != "Tokyo" || res.Mode != OutputNative || res.Attempts != 1 || string(res.JSON) != `{"city": "Tokyo"}` {
		t.Fatalf("got %+v, result %+v", got, res)
	}
	if !slices.Equal(p.calls, []string{"schema"}) {
		t.Fatalf("calls = %v, want one schema call", p.calls)
	}
	if !slices.Equal(partials, []string{`{"city":"Tok"}`, `{"city":"Tokyo"}`}) {
		t.Fatalf("partials = %q", partials)
	}
	// The scoped schema does not outlive the call.
	if a.output(context.Background()).schema != nil {
		t.Fatal("the agent kept the scoped schema")
	}
}

func TestStructuredFinalAnswerTool(t *testing.T) {
	tests := []struct {
		name      string
		responses [][]types.Delta
		wantCalls int
		wantErr   bool
	}{
		{
			name: "tools and schema in the same request",
			responses: [][]types.Delta{
				agenttest.ToolCallResponse("c1", "lookup", nil),
				agenttest.ToolCallResponse("c2", FinalAnswerToolName, map[string]any{"city": "Tokyo"}),
			},
			wantCalls: 2,
		},
		{
			name: "invalid arguments are corrected inside the run",
			responses: [][]types.Delta{
				agenttest.ToolCallResponse("c1", FinalAnswerToolName, map[string]any{"city": 3}),
				agenttest.ToolCallResponse("c2", FinalAnswerToolName, map[string]any{"city": "Tokyo"}),
			},
			wantCalls: 2,
		},
		{
			name: "a validator error goes back as a tool error",
			responses: [][]types.Delta{
				agenttest.ToolCallResponse("c1", FinalAnswerToolName, map[string]any{"city": "Atlantis"}),
				agenttest.ToolCallResponse("c2", FinalAnswerToolName, map[string]any{"city": "Tokyo"}),
			},
			wantCalls: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lookup := &agenttest.MockTool{Def: types.ToolDef{Name: "lookup"}, Result: "Tokyo"}
			p := &agenttest.ScriptedProvider{Responses: tt.responses}
			a := must.Get(New(Config{Provider: p, Tools: types.NewToolRegistry(lookup)}))
			got, res, err := Structured(context.Background(), a, []types.Message{types.UserMsg(types.Text("Where?"))}, OutputSpec[city]{
				Validate: func(c city) error {
					if c.City == "Atlantis" {
						return errors.New("not a real city")
					}
					return nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if got.City != "Tokyo" || res.Mode != OutputTool || res.Attempts != 1 {
				t.Fatalf("got %+v, result %+v", got, res)
			}
			if p.CallCount() != tt.wantCalls {
				t.Fatalf("provider calls = %d, want %d", p.CallCount(), tt.wantCalls)
			}
			for i, req := range p.Requests() {
				if !slices.ContainsFunc(req.Tools, func(d types.ToolDef) bool { return d.Name == FinalAnswerToolName }) {
					t.Fatalf("request %d did not offer %s", i, FinalAnswerToolName)
				}
			}
		})
	}
}

func TestStructuredRepairsToolErrorLimit(t *testing.T) {
	bad := map[string]any{"city": 3}
	p := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("c1", FinalAnswerToolName, bad),
		agenttest.ToolCallResponse("c2", FinalAnswerToolName, bad),
		agenttest.ToolCallResponse("c3", FinalAnswerToolName, map[string]any{"city": "Tokyo"}),
	}}
	a := must.Get(New(Config{Provider: p}))
	got, res, err := Structured(context.Background(), a, []types.Message{types.UserMsg(types.Text("Where?"))}, OutputSpec[city]{Mode: OutputTool, Repair: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got.City != "Tokyo" || res.Attempts != 2 {
		t.Fatalf("got %+v after %d attempts", got, res.Attempts)
	}
}

func TestStructuredPrompt(t *testing.T) {
	p := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.TextResponse("<think>{\"city\":\"Paris\"}</think>Here:\n```json\n{\"city\":\"Tokyo\"}\n```"),
	}}
	a := must.Get(New(Config{Provider: p, SystemPrompt: "base"}))
	got, res, err := Structured(context.Background(), a, []types.Message{types.UserMsg(types.Text("Where?"))}, OutputSpec[city]{Mode: OutputPrompt})
	if err != nil {
		t.Fatal(err)
	}
	if got.City != "Tokyo" || res.Mode != OutputPrompt {
		t.Fatalf("got %+v, result %+v", got, res)
	}
	req := lastRequestText(p.Requests()[0].Messages)
	if !strings.Contains(req, "base") || !strings.Contains(req, "JSON Schema") || !strings.Contains(req, `"city"`) {
		t.Fatalf("request does not carry the schema instruction: %q", req)
	}
	root := a.Tree().Root().Message.(types.SystemMessage)
	if len(root.Parts) != 1 {
		t.Fatal("the instruction was written into the tree")
	}
}

func TestStructuredRepair(t *testing.T) {
	tests := []struct {
		name         string
		responses    []string
		repair       int
		wantAttempts int
		wantErr      error
		wantCity     string
	}{
		{"repaired after bad JSON", []string{"I think Tokyo", `{"city":"Tokyo"}`}, 1, 2, nil, "Tokyo"},
		{"repaired after schema mismatch", []string{`{"city":1}`, `{"city":"Tokyo"}`}, 2, 2, nil, "Tokyo"},
		{"repaired after validator rejection", []string{`{"city":"Atlantis"}`, `{"city":"Tokyo"}`}, 1, 2, nil, "Tokyo"},
		{"gives up when repairs run out", []string{"no", "still no"}, 1, 2, ErrSchemaInvalid, ""},
		{"no repair by default", []string{"no"}, 0, 1, ErrSchemaInvalid, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var script [][]types.Delta
			for _, r := range tt.responses {
				script = append(script, agenttest.TextResponse(r))
			}
			p := &schemaProvider{schema: script}
			a := must.Get(New(Config{Provider: p}))
			got, res, err := Structured(context.Background(), a, []types.Message{types.UserMsg(types.Text("Where?"))}, OutputSpec[city]{
				Repair: tt.repair,
				Validate: func(c city) error {
					if c.City == "Atlantis" {
						return errors.New("not a real city")
					}
					return nil
				},
			})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if res.Attempts != tt.wantAttempts || got.City != tt.wantCity {
				t.Fatalf("got %+v after %d attempts; want %q after %d", got, res.Attempts, tt.wantCity, tt.wantAttempts)
			}
			if tt.wantAttempts > 1 && !strings.Contains(lastRequestText(p.lastMsgs), "was not accepted") {
				t.Fatal("the repair request does not explain the failure")
			}
		})
	}
}

func TestStructuredOutcomeSwitch(t *testing.T) {
	p := newSwitchingProvider("small", [][]types.Delta{
		agenttest.TextResponse("no"),
		agenttest.TextResponse(`{"city":"Tokyo"}`),
	})
	var seen []types.Outcome
	ladder := types.EscalationLadder{Models: []string{"small", "large"}, Kinds: []types.OutcomeKind{types.OutcomeSchemaInvalid}}
	policy := types.OutcomePolicyFunc(func(ctx context.Context, o types.Outcome) (*types.Switch, error) {
		seen = append(seen, o)
		return ladder.Observe(ctx, o)
	})
	a := must.Get(New(Config{Provider: p, Name: "worker"}, WithOutcomePolicy(policy)))
	var routes []types.RouteDelta
	got, res, err := Structured(context.Background(), a, []types.Message{types.UserMsg(types.Text("Where?"))}, OutputSpec[city]{
		OnDelta: func(d types.Delta) {
			if r, ok := d.(types.RouteDelta); ok {
				routes = append(routes, r)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.City != "Tokyo" || res.Attempts != 2 || len(res.Switches) != 1 || res.Switches[0].Model != "large" {
		t.Fatalf("got %+v, result %+v", got, res)
	}
	if !slices.Equal(*p.models, []string{"small", "large"}) {
		t.Fatalf("models = %v, want small then large", *p.models)
	}
	if len(seen) != 1 || seen[0].Kind != types.OutcomeSchemaInvalid || seen[0].Agent != "worker" || seen[0].Model != "small" || seen[0].Err == nil {
		t.Fatalf("outcomes = %+v", seen)
	}
	if len(routes) != 1 || routes[0].Model != "large" || routes[0].Reason != string(types.OutcomeSchemaInvalid) {
		t.Fatalf("routes = %+v", routes)
	}
	msgs, _ := a.Tree().FlattenBranch(a.Tree().Active())
	found := false
	for _, m := range msgs {
		if sm, ok := m.(types.SystemMessage); ok {
			for _, c := range sm.Parts {
				if cc, ok := c.(types.ConfigPart); ok && cc.Target.Model == "large" && cc.Reason == string(types.OutcomeSchemaInvalid) {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("the switch was not recorded as ConfigContent")
	}
}

func TestStructuredOutcomeSwitchStopsAtTop(t *testing.T) {
	p := newSwitchingProvider("large", [][]types.Delta{agenttest.TextResponse("no")})
	a := must.Get(New(Config{Provider: p}, WithOutcomePolicy(types.EscalationLadder{Models: []string{"small", "large"}})))
	_, res, err := Structured(context.Background(), a, []types.Message{types.UserMsg(types.Text("Where?"))}, OutputSpec[city]{})
	if !errors.Is(err, ErrSchemaInvalid) || len(res.Switches) != 0 {
		t.Fatalf("err = %v, switches = %v", err, res.Switches)
	}
}

func TestStructuredModeSelection(t *testing.T) {
	lookup := &agenttest.MockTool{Def: types.ToolDef{Name: "lookup"}}
	taken := &agenttest.MockTool{Def: types.ToolDef{Name: FinalAnswerToolName}}
	tests := []struct {
		name     string
		provider types.Provider
		tools    []types.Tool
		mode     OutputMode
		schema   *types.ParameterSchema
		want     OutputMode
		wantErr  bool
	}{
		{"native provider without tools", &schemaProvider{}, nil, OutputAuto, nil, OutputNative, false},
		{"native provider with tools", &schemaProvider{}, []types.Tool{lookup}, OutputAuto, nil, OutputTool, false},
		{"no native support", &agenttest.ScriptedProvider{}, nil, OutputAuto, nil, OutputTool, false},
		{"neither native nor tools", &noSchemaModel{}, nil, OutputAuto, nil, OutputPrompt, false},
		{"name taken falls back to native", &schemaProvider{}, []types.Tool{taken}, OutputAuto, nil, OutputNative, false},
		{"withdrawn structured output uses the tool", &withdrawnSchemaModel{}, nil, OutputAuto, nil, OutputTool, false},
		{"explicit native after withdrawal", &withdrawnSchemaModel{}, nil, OutputNative, nil, "", true},
		{"explicit native without support", &agenttest.ScriptedProvider{}, nil, OutputNative, nil, "", true},
		{"explicit tool without tool support", &noSchemaModel{}, nil, OutputTool, nil, "", true},
		{"tool mode needs an object", &agenttest.ScriptedProvider{}, nil, OutputTool, &types.ParameterSchema{Type: "array"}, "", true},
		{"unknown mode", &agenttest.ScriptedProvider{}, nil, OutputMode("xml"), nil, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := must.Get(New(Config{Provider: tt.provider, Tools: types.NewToolRegistry(tt.tools...)}))
			schema := tt.schema
			if schema == nil {
				schema = cityPopulationSchema
			}
			got, err := a.resolveOutputMode(tt.mode, schema)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if !errors.Is(err, types.ErrInvalidModelConfig) {
					t.Fatalf("err = %v, want ErrInvalidModelConfig", err)
				}
				return
			}
			if got != tt.want {
				t.Fatalf("mode = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStructuredSchemaRequiredForNonStruct(t *testing.T) {
	p := &schemaProvider{schema: [][]types.Delta{agenttest.TextResponse(`{"city":"Tokyo"}`)}}
	a := must.Get(New(Config{Provider: p}))
	in := []types.Message{types.UserMsg(types.Text("Where?"))}
	if _, _, err := Structured(context.Background(), a, in, OutputSpec[map[string]any]{}); !errors.Is(err, types.ErrInvalidModelConfig) {
		t.Fatalf("err = %v, want ErrInvalidModelConfig", err)
	}
	got, _, err := Structured(context.Background(), a, in, OutputSpec[map[string]any]{Schema: cityPopulationSchema})
	if err != nil || got["city"] != "Tokyo" {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestStructuredRunError(t *testing.T) {
	p := &agenttest.ScriptedProvider{Errors: []error{errors.New("boom")}}
	a := must.Get(New(Config{Provider: p}))
	_, res, err := Structured(context.Background(), a, []types.Message{types.UserMsg(types.Text("Where?"))}, OutputSpec[city]{Repair: 3})
	if err == nil || errors.Is(err, ErrSchemaInvalid) || res.Attempts != 1 {
		t.Fatalf("err = %v after %d attempts; a run error must not be repaired", err, res.Attempts)
	}
}
