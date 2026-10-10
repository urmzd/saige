package agenttest_test

import (
	"context"
	"fmt"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
)

func ExampleFunctionModel() {
	model := &agenttest.FunctionModel{
		Fn: func(_ context.Context, _ []types.Message, tools []types.ToolDef, req agenttest.Request) (agenttest.Response, error) {
			return agenttest.Response{
				Text:         fmt.Sprintf("call %d, %d tools offered", req.Index, len(tools)),
				Usage:        &types.UsageDelta{PromptTokens: 12, CompletionTokens: 4},
				FinishReason: "stop",
			}, nil
		},
	}
	a := agent.NewAgent(agent.AgentConfig{Provider: model})
	text, _ := agent.CollectText(a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("hi"))}))
	fmt.Println(text)
	// Output: call 0, 0 tools offered
}

func ExampleNewToolCallEmulator() {
	weather := agent.Func("weather", "current weather", func(_ agent.RunContext[agent.NoDeps], in struct {
		City string `json:"city"`
	}) (string, error) {
		return "sunny in " + in.City, nil
	})
	model := agenttest.NewToolCallEmulator(agenttest.EmulatorConfig{})
	a := agent.NewAgent(agent.AgentConfig{Provider: model, Tools: types.NewToolRegistry(weather)})
	text, _ := agent.CollectText(a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("weather?"))}))
	fmt.Println(text)
	// Output: weather: sunny in sample city
}
