package agenttest_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

func ptr[T any](v T) *T { return &v }

func TestFunctionModelStreamsAResponse(t *testing.T) {
	m := &agenttest.FunctionModel{Fn: func(context.Context, []types.Message, []types.ToolDef, agenttest.Request) (agenttest.Response, error) {
		return agenttest.Response{
			Thinking:     "hmm",
			TextChunks:   []string{"Hel", "lo"},
			ToolCalls:    []types.ToolCallPart{{Name: "greet", Arguments: map[string]any{"name": "Ada"}}},
			Usage:        &types.UsageDelta{PromptTokens: 10, CompletionTokens: 3},
			FinishReason: "tool_use",
		}, nil
	}}
	ch, err := m.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("hi"))}})
	if err != nil {
		t.Fatal(err)
	}
	deltas := agenttest.CollectDeltas(ch)
	var kinds []string
	var usage types.UsageDelta
	for _, d := range deltas {
		switch v := d.(type) {
		case types.PartDelta:
			switch {
			case v.Thinking != "":
				kinds = append(kinds, "thinking:"+v.Thinking)
			case v.Text != "":
				kinds = append(kinds, "text:"+v.Text)
			case v.Args != "":
				kinds = append(kinds, "args:"+v.Args)
			}
		case types.PartStart:
			if v.Kind == types.KindToolCall {
				kinds = append(kinds, "call:"+v.ID+":"+v.Name)
			}
		case types.UsageDelta:
			usage = v
			kinds = append(kinds, "usage")
		}
	}
	want := []string{"thinking:hmm", "text:Hel", "text:lo", "call:call_0_0:greet", `args:{"name":"Ada"}`, "usage"}
	if !slices.Equal(kinds, want) {
		t.Fatalf("stream = %v, want %v", kinds, want)
	}
	if usage.TotalTokens != 13 || !slices.Equal(usage.FinishReasons, []string{"tool_use"}) {
		t.Fatalf("usage = %+v", usage)
	}
	if calls := agenttest.CollectToolCalls(mustStream(t, m)); len(calls) != 1 || calls[0].Arguments["name"] != "Ada" {
		t.Fatalf("tool calls = %+v", calls)
	}
}

func mustStream(t *testing.T, p types.Provider) <-chan types.Delta {
	t.Helper()
	ch, err := p.Stream(context.Background(), types.Request{})
	if err != nil {
		t.Fatal(err)
	}
	return ch
}

func TestFunctionModelErrors(t *testing.T) {
	boom := errors.New("boom")
	m := &agenttest.FunctionModel{Fn: func(_ context.Context, _ []types.Message, _ []types.ToolDef, req agenttest.Request) (agenttest.Response, error) {
		if req.Index == 0 {
			return agenttest.Response{}, boom
		}
		return agenttest.Response{Text: "partial", StreamErr: boom}, nil
	}}
	if _, err := m.Stream(context.Background(), types.Request{}); !errors.Is(err, boom) {
		t.Fatalf("first call err = %v", err)
	}
	deltas := agenttest.CollectDeltas(mustStream(t, m))
	last, ok := deltas[len(deltas)-1].(types.ErrorDelta)
	if !ok || !errors.Is(last.Error, boom) || agenttest.CollectText(mustStream(t, m)) != "partial" {
		t.Fatalf("stream = %+v", deltas)
	}
	if m.CallCount() != 3 {
		t.Fatalf("calls = %d", m.CallCount())
	}
}

func TestFunctionModelSeesOptionsAndDialReport(t *testing.T) {
	var got agenttest.Request
	m := &agenttest.FunctionModel{
		Options: types.RequestOptions{MaxOutputTokens: ptr(int64(256))},
		Caps: &types.ModelCapabilities{
			Provider: "function", Model: "function-model",
			Caps: map[types.Capability]bool{types.CapStreaming: true, types.CapTemperature: true,
				types.CapMaxOutputTokens: true, types.CapTools: true, types.CapToolChoice: true},
		},
		Fn: func(_ context.Context, _ []types.Message, _ []types.ToolDef, req agenttest.Request) (agenttest.Response, error) {
			got = req
			return agenttest.Response{Text: "ok"}, nil
		},
	}
	creative := types.CreativityDeterministic
	_, err := m.Stream(context.Background(), types.Request{Options: &types.RequestOptions{
		Dials: types.Dials{Creativity: &creative},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Options == nil || got.Options.Dials.Creativity == nil {
		t.Fatalf("request options = %+v", got.Options)
	}
	if got.Effective.MaxOutputTokens == nil || *got.Effective.MaxOutputTokens != 256 || got.Effective.Temperature == nil {
		t.Fatalf("effective = %+v", got.Effective)
	}
	if got.Dials == nil {
		t.Fatal("no dial report")
	}
	if d, ok := got.Dials.Decision(types.DialCreativity); !ok || d.Action == types.DialRejected {
		t.Fatalf("creativity decision = %+v", d)
	}
}

func TestFunctionModelChecksDeclaredCapabilities(t *testing.T) {
	m := &agenttest.FunctionModel{Caps: &types.ModelCapabilities{
		Provider: "function", Model: "no-tools", Known: true,
		Caps: map[types.Capability]bool{types.CapStreaming: true},
	}}
	tools := []types.ToolDef{{Name: "t", Parameters: types.ParameterSchema{Type: types.SchemaObject}}}
	if _, err := m.Stream(context.Background(), types.Request{Tools: tools}); !errors.Is(err, types.ErrInvalidModelConfig) {
		t.Fatalf("tools on a model without tools: err = %v", err)
	}
	if _, err := m.Stream(context.Background(), types.Request{Options: &types.RequestOptions{Temperature: ptr(0.5)}}); !errors.Is(err, types.ErrInvalidModelConfig) {
		t.Fatalf("temperature on a model without it: err = %v", err)
	}
	if _, err := m.Stream(context.Background(), types.Request{Schema: &types.ParameterSchema{Type: types.SchemaObject}}); !errors.Is(err, types.ErrInvalidModelConfig) {
		t.Fatalf("schema on a model without structured output: err = %v", err)
	}
	// The agent refuses a schema the declared model cannot enforce, before
	// any call.
	a := must.Get(agent.New(agent.Config{Provider: m}, agent.WithResponseSchema(&types.ParameterSchema{Type: types.SchemaObject})))
	if _, err := agent.CollectText(a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("hi"))})); err == nil {
		t.Fatal("the agent sent a schema to a model without structured output")
	}
	if n := m.CallCount(); n != 3 {
		t.Fatalf("calls = %d, want only the three direct ones", n)
	}
}

func TestFunctionModelReportsCatalogCapabilities(t *testing.T) {
	m := &agenttest.FunctionModel{CatalogModel: "anthropic/claude-haiku-4-5"}
	caps, ok := types.ProviderCapabilities(m)
	if !ok || caps.Provider != "anthropic" || !caps.Supports(types.CapTools) || caps.Family == "" {
		t.Fatalf("caps = %+v", caps)
	}
	if types.ProviderModel(m) != "claude-haiku-4-5" {
		t.Fatalf("model = %q", types.ProviderModel(m))
	}
	// Without a declaration the model reports the permissive default, which
	// is not an exact declaration.
	def, _ := types.ProviderCapabilities(&agenttest.FunctionModel{})
	if def.Known || !def.Supports(types.CapStructuredOutput) || !def.Supports(types.CapToolChoice) {
		t.Fatalf("default caps = %+v", def)
	}
}

func TestFunctionModelDrivesAnAgent(t *testing.T) {
	var mu sync.Mutex
	var seen []agenttest.Request
	greet := &agenttest.MockTool{
		Def: types.ToolDef{Name: "greet", Parameters: types.ParameterSchema{Type: types.SchemaObject,
			Required: []string{"name"}, Properties: map[string]types.PropertyDef{"name": {Type: types.SchemaString}}}},
		Result: "Hello, Ada!",
	}
	m := &agenttest.FunctionModel{Fn: func(_ context.Context, msgs []types.Message, tools []types.ToolDef, req agenttest.Request) (agenttest.Response, error) {
		mu.Lock()
		seen = append(seen, req)
		mu.Unlock()
		// Answer once the tool result is in the conversation.
		if _, ok := msgs[len(msgs)-1].(types.SystemMessage); ok {
			return agenttest.Response{Text: "greeted", Usage: &types.UsageDelta{PromptTokens: 1, CompletionTokens: 1}}, nil
		}
		if len(tools) != 1 || tools[0].Name != "greet" {
			return agenttest.Response{}, errors.New("greet not offered")
		}
		return agenttest.Response{ToolCalls: []types.ToolCallPart{{Name: "greet", Arguments: map[string]any{"name": "Ada"}}}}, nil
	}}
	a := must.Get(agent.New(agent.Config{Provider: m, Tools: types.NewToolRegistry(greet)},
		agent.WithToolChoice(types.ToolChoice{Mode: types.ToolChoiceRequired})))
	text, err := agent.CollectText(a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("greet Ada"))}))
	if err != nil {
		t.Fatal(err)
	}
	if text != "greeted" || greet.CallCount() != 1 || greet.Calls[0]["name"] != "Ada" {
		t.Fatalf("text = %q, tool calls %+v", text, greet.Calls)
	}
	// The forced tool choice reached the first call as a request option.
	if len(seen) < 2 || seen[0].Options == nil || seen[0].Options.ToolChoice == nil || seen[0].Options.ToolChoice.Mode != types.ToolChoiceRequired {
		t.Fatalf("requests = %+v", seen)
	}
}

func TestFunctionModelServesStructuredOutput(t *testing.T) {
	type city struct {
		City string `json:"city"`
	}
	m := &agenttest.FunctionModel{Fn: func(_ context.Context, _ []types.Message, _ []types.ToolDef, req agenttest.Request) (agenttest.Response, error) {
		if req.Schema == nil {
			return agenttest.Response{Text: "no schema"}, nil
		}
		if _, ok := req.Schema.Properties["city"]; !ok {
			return agenttest.Response{}, errors.New("wrong schema")
		}
		return agenttest.Response{Text: `{"city": "Paris"}`}, nil
	}}
	got, res, err := agent.Structured(context.Background(), must.Get(agent.New(agent.Config{Provider: m})),
		[]types.Message{types.UserMsg(types.Text("Where?"))}, agent.OutputSpec[city]{})
	if err != nil {
		t.Fatal(err)
	}
	if got.City != "Paris" || res.Mode != agent.OutputNative {
		t.Fatalf("got %+v, mode %v", got, res.Mode)
	}
	if reqs := m.Requests(); len(reqs) != 1 || reqs[0].Request.Schema == nil || !strings.Contains(lastUser(reqs[0].Messages), "Where?") {
		t.Fatalf("requests = %+v", reqs)
	}
}

func lastUser(msgs []types.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if um, ok := msgs[i].(types.UserMessage); ok {
			for _, c := range um.Parts {
				if tc, ok := c.(types.TextPart); ok {
					return tc.Text
				}
			}
		}
	}
	return ""
}
