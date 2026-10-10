// The smallest agent: a catalog preset instead of a hand-built adapter.
// "default" serves the cheapest model of each vendor whose credentials are
// set (Anthropic, OpenAI, Google), then a local Ollama model, failing over in
// that order.
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

	a := agent.NewAgent(agent.AgentConfig{SystemPrompt: "Answer in one sentence."}, agent.WithPreset(bundle))
	text, err := agent.CollectText(a.Invoke(ctx, []types.Message{types.NewUserMessage("What is RAG?")}))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(text)
}
