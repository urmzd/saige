// Dials: model-neutral intents compiled for whichever model serves the call,
// so the same setting works on every vendor and survives a failover.
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/provider/preset"
	"github.com/urmzd/saige/agent/types"
)

func main() {
	ctx := context.Background()
	bundle, err := preset.Build(ctx, catalog.Default(), "default", nil, preset.Options{})
	if err != nil {
		log.Fatal(err)
	}
	defer bundle.Close()

	a := agent.NewAgent(agent.AgentConfig{}, agent.WithPreset(bundle),
		agent.WithDials(types.Dials{Creativity: new(types.CreativityFocused), Reasoning: &types.ReasoningDial{Depth: types.DepthHigh}}))
	text, err := agent.CollectText(a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("Is 2^61 - 1 prime? Answer yes or no."))}))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(text)
}
