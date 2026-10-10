// Package main demonstrates a simple chat agent with a single "add" tool
// that adds two numbers. It creates an Ollama-backed agent, invokes it with
// a math question, and streams the response deltas to stdout.
package main

import (
	"context"
	"fmt"
	"log"

	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/provider/ollama"
	"github.com/urmzd/saige/agent/types"
)

func main() {
	// Create Ollama client and adapter.
	client, err := ollama.NewClient(ollama.Config{Host: "http://localhost:11434", Model: "qwen3.5:4b"})
	if err != nil {
		log.Fatal(err)
	}
	adapter, err := ollama.New(ollama.Config{Client: client})
	if err != nil {
		log.Fatal(err)
	}

	// Define an "add" tool that sums two numbers.
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

	// Build the agent.
	agent, err := agentsdk.New(agentsdk.Config{
		Name:         "calculator",
		SystemPrompt: "You are a helpful calculator. Use the add tool to perform addition.",
		Provider:     adapter,
		Tools:        types.NewToolRegistry(addTool),
	})
	if err != nil {
		log.Fatal(err)
	}

	// Invoke with a user message.
	stream := agent.Invoke(context.Background(), []types.Message{
		types.UserMsg(types.Text("What is 2 + 3?")),
	})

	// Stream deltas and print text content.
	for delta := range stream.Deltas() {
		switch d := delta.(type) {
		case types.PartDelta:
			fmt.Print(d.Text)
		case types.ErrorDelta:
			log.Fatal(d.Error)
		case types.DoneDelta:
			fmt.Println()
		}
	}

	if err := stream.Wait(); err != nil {
		log.Fatal(err)
	}
}
