// The smallest agent: a catalog preset instead of a hand-built adapter.
// "default" serves the cheapest model of each vendor whose credentials are
// set (Anthropic, OpenAI, Google), then a model pulled into a local Ollama,
// failing over in that order.
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
	defer bundle.Close(ctx)

	a, err := agent.New(agent.Config{SystemPrompt: "Answer in one sentence."}, agent.WithPreset(bundle))
	if err != nil {
		log.Fatal(err)
	}
	text, err := agent.CollectText(a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("What is RAG?"))}))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(text)
}
