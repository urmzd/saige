// Package main demonstrates the Runner pattern for multi-turn interactive
// conversations. This is the "out-of-the-box" tier: create an agent, create
// a runner, call agentsdk.Run().
//
// Usage:
//
//	go run ./examples/runner/             # interactive bubbletea mode
//	go run ./examples/runner/ -verbose    # plain text mode (no TTY required)
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"

	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/provider/ollama"
	"github.com/urmzd/saige/agent/tui"
	"github.com/urmzd/saige/agent/types"
)

func main() {
	verbose := len(os.Args) > 1 && os.Args[1] == "-verbose"

	client := ollama.NewClient("http://localhost:11434", "qwen3.5:4b", "")
	adapter := ollama.NewAdapter(client)

	addTool := &types.ToolFunc{
		Def: types.ToolDef{
			Name:        "add",
			Description: "Add two numbers together",
			Parameters: types.ParameterSchema{
				Type:     "object",
				Required: []string{"a", "b"},
				Properties: map[string]types.PropertyDef{
					"a": {Type: "number", Description: "First number"},
					"b": {Type: "number", Description: "Second number"},
				},
			},
		},
		Fn: func(ctx context.Context, args map[string]any) (string, error) {
			a, _ := args["a"].(float64)
			b, _ := args["b"].(float64)
			return fmt.Sprintf("%g", a+b), nil
		},
	}

	agent := agentsdk.NewAgent(agentsdk.AgentConfig{
		Name:         "calculator",
		SystemPrompt: "You are a helpful calculator. Use the add tool when asked to add numbers.",
		Provider:     adapter,
		Tools:        types.NewToolRegistry(addTool),
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	runner := &tui.Runner{
		Title:   "Calculator Agent",
		Verbose: verbose,
	}

	if err := agentsdk.Run(ctx, agent, runner); err != nil {
		log.Fatal(err)
	}
}
