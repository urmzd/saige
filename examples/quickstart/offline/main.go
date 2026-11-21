// A scripted provider supplies a known response so this example works offline.
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
)

func main() {
	provider := &agenttest.ScriptedProvider{
		Responses: [][]types.Delta{agenttest.TextResponse("Hello from Saige.")},
	}
	a, err := agent.New(agent.Config{Provider: provider})
	if err != nil {
		log.Fatal(err)
	}

	answer, err := agent.CollectText(a.Prompt(context.Background(), "Say hello."))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(answer)
}
