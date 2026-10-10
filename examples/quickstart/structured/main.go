// Structured output: agent.Structured decodes the answer into a Go struct,
// checks it against the schema derived from the struct, and asks the model
// to repair an invalid answer.
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

type Triage struct {
	Priority string `json:"priority" enum:"low,medium,high"`
	Summary  string `json:"summary"`
}

func main() {
	ctx := context.Background()
	bundle, err := preset.Build(ctx, catalog.Default(), "default", nil, preset.Options{})
	if err != nil {
		log.Fatal(err)
	}
	defer bundle.Close()

	a := agent.New(agent.Config{SystemPrompt: "Triage support tickets."}, agent.WithPreset(bundle))
	ticket := []types.Message{types.UserMsg(types.Text("Checkout returns HTTP 500 for every customer."))}
	out, _, err := agent.Structured(ctx, a, ticket, agent.OutputSpec[Triage]{Repair: 1})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s: %s\n", out.Priority, out.Summary)
}
