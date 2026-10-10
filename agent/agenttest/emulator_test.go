package agenttest_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

type searchInput struct {
	Query  string   `json:"query" description:"what to look for"`
	Limit  int      `json:"limit,omitempty"`
	Sort   string   `json:"sort,omitempty" enum:"relevance,date"`
	Tags   []string `json:"tags,omitempty"`
	Filter *struct {
		Exact bool    `json:"exact"`
		Score float64 `json:"score,omitempty"`
	} `json:"filter,omitempty"`
}

func searchTool(calls *[]searchInput) types.Tool {
	return agent.Func("search", "search the index", func(_ agent.RunContext[agent.NoDeps], in searchInput) (string, error) {
		*calls = append(*calls, in)
		return "3 hits for " + in.Query, nil
	}, agent.Capability(types.ToolCapabilityRead))
}

func TestToolCallEmulatorDrivesTheToolLoop(t *testing.T) {
	var searches []searchInput
	clock := &agenttest.MockTool{
		Def:    types.ToolDef{Name: "clock", Parameters: types.ParameterSchema{Type: types.SchemaObject}},
		Result: "12:00",
	}
	em := agenttest.NewToolCallEmulator(agenttest.EmulatorConfig{})
	a := must.Get(agent.New(agent.Config{Provider: em, Tools: types.NewToolRegistry(searchTool(&searches), clock)}))
	text, err := agent.CollectText(a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("go"))}))
	if err != nil {
		t.Fatal(err)
	}
	// Strict decoding in agent.Func accepted the generated arguments.
	if len(searches) != 1 || clock.CallCount() != 1 {
		t.Fatalf("searches = %+v, clock calls %d", searches, clock.CallCount())
	}
	in := searches[0]
	if in.Query == "" || in.Limit != 1 || in.Sort != "relevance" || len(in.Tags) != 1 || in.Filter == nil || !in.Filter.Exact {
		t.Fatalf("search arguments = %+v", in)
	}
	for _, line := range []string{"search: 3 hits for sample query", "clock: 12:00"} {
		if !strings.Contains(text, line) {
			t.Errorf("answer %q lacks %q", text, line)
		}
	}
	if em.CallCount() != 2 {
		t.Fatalf("model calls = %d, want a tool turn and an answer", em.CallCount())
	}
}

func TestToolCallEmulatorOptions(t *testing.T) {
	var searches []searchInput
	clock := &agenttest.MockTool{Def: types.ToolDef{Name: "clock", Parameters: types.ParameterSchema{Type: types.SchemaObject}}}
	em := agenttest.NewToolCallEmulator(agenttest.EmulatorConfig{
		Tools: []string{"search"}, Rounds: 2, RequiredOnly: true, Answer: "finished",
		Usage: &types.UsageDelta{PromptTokens: 5, CompletionTokens: 2},
	})
	a := must.Get(agent.New(agent.Config{Provider: em, Tools: types.NewToolRegistry(searchTool(&searches), clock)}))
	tr, err := agent.Collect(a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("go"))}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Text != "finished" || len(searches) != 2 || clock.CallCount() != 0 {
		t.Fatalf("text %q, searches %+v, clock %d", tr.Text, searches, clock.CallCount())
	}
	if !reflect.DeepEqual(searches[0], searchInput{Query: "sample query"}) {
		t.Fatalf("required-only arguments = %+v", searches[0])
	}
	if tr.Usage.TotalTokens != 21 {
		t.Fatalf("usage = %+v", tr.Usage)
	}
}

func TestToolCallEmulatorHonorsToolChoice(t *testing.T) {
	var searches []searchInput
	clock := &agenttest.MockTool{Def: types.ToolDef{Name: "clock", Parameters: types.ParameterSchema{Type: types.SchemaObject}}, Result: "12:00"}
	tools := types.NewToolRegistry(searchTool(&searches), clock)

	em := agenttest.NewToolCallEmulator(agenttest.EmulatorConfig{})
	a := must.Get(agent.New(agent.Config{Provider: em, Tools: tools},
		agent.WithToolChoice(types.ToolChoice{Mode: types.ToolChoiceNamed, Name: "clock"})))
	if _, err := agent.CollectText(a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("go"))})); err != nil {
		t.Fatal(err)
	}
	if clock.CallCount() != 1 || len(searches) != 0 {
		t.Fatalf("named choice: clock %d, searches %d", clock.CallCount(), len(searches))
	}

	em = agenttest.NewToolCallEmulator(agenttest.EmulatorConfig{Answer: "no tools"})
	a = must.Get(agent.New(agent.Config{Provider: em, Tools: tools},
		agent.WithToolChoice(types.ToolChoice{Mode: types.ToolChoiceNone})))
	text, err := agent.CollectText(a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("go"))}))
	if err != nil || text != "no tools" || clock.CallCount() != 1 {
		t.Fatalf("none choice: text %q, err %v, clock %d", text, err, clock.CallCount())
	}
}

func TestToolCallEmulatorAnswersASchema(t *testing.T) {
	type report struct {
		Title string   `json:"title"`
		Count int      `json:"count"`
		Tags  []string `json:"tags"`
	}
	em := agenttest.NewToolCallEmulator(agenttest.EmulatorConfig{})
	got, _, err := agent.Structured(context.Background(), must.Get(agent.New(agent.Config{Provider: em})),
		[]types.Message{types.UserMsg(types.Text("report"))}, agent.OutputSpec[report]{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Title == "" || got.Count != 1 || len(got.Tags) != 1 {
		t.Fatalf("report = %+v", got)
	}
}

func TestToolCallEmulatorReportsCatalogCapabilities(t *testing.T) {
	em := agenttest.NewToolCallEmulator(agenttest.EmulatorConfig{})
	em.Caps = &types.ModelCapabilities{Provider: "emulator", Model: "no-tools", Known: true,
		Caps: map[types.Capability]bool{types.CapStreaming: true}}
	clock := &agenttest.MockTool{Def: types.ToolDef{Name: "clock", Parameters: types.ParameterSchema{Type: types.SchemaObject}}}
	a := must.Get(agent.New(agent.Config{Provider: em, Tools: types.NewToolRegistry(clock)}))
	if _, err := agent.CollectText(a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("go"))})); err == nil {
		t.Fatal("a model declared without tools was offered tools")
	}
	if clock.CallCount() != 0 {
		t.Fatal("the tool ran")
	}
}

func TestFakeArgumentsAreSchemaValidAndDeterministic(t *testing.T) {
	schemas := []types.ParameterSchema{
		types.SchemaFrom[searchInput](),
		{Type: types.SchemaObject, Required: []string{"undeclared"}},
		{Type: types.SchemaObject, Properties: map[string]types.PropertyDef{
			"level":  {Type: types.SchemaInteger, Enum: []string{"3", "5"}},
			"ratio":  {Type: types.SchemaNumber, Default: 0.25},
			"flag":   {Type: types.SchemaBoolean, Nullable: true},
			"matrix": {Type: types.SchemaArray, Items: &types.PropertyDef{Type: types.SchemaArray, Items: &types.PropertyDef{Type: types.SchemaInteger}}},
			"nested": {Type: types.SchemaObject, Required: []string{"id"}, Properties: map[string]types.PropertyDef{"id": {Type: types.SchemaString}}},
		}},
	}
	for i, s := range schemas {
		for _, all := range []bool{true, false} {
			args := agenttest.FakeArguments(s, all)
			if err := types.ValidateToolArgs(s, args); err != nil {
				t.Errorf("schema %d (all=%v): %v; args %v", i, all, err, args)
			}
			if again := agenttest.FakeArguments(s, all); !reflect.DeepEqual(args, again) {
				t.Errorf("schema %d: not deterministic: %v vs %v", i, args, again)
			}
			if _, err := json.Marshal(args); err != nil {
				t.Errorf("schema %d: %v", i, err)
			}
		}
	}
	args := agenttest.FakeArguments(schemas[2], true)
	if args["level"] != float64(3) || args["ratio"] != 0.25 || args["flag"] != true {
		t.Fatalf("args = %v", args)
	}
	if got := agenttest.FakeArguments(schemas[1], false); got["undeclared"] == nil {
		t.Fatalf("an undeclared required property was left out: %v", got)
	}
}
