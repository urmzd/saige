// Package main demonstrates a parent agent delegating work to a child
// "researcher" sub-agent. The researcher has a mock "search" tool. The parent
// agent routes tasks to the researcher via the automatic delegate_to_ tool,
// and child deltas are forwarded with ToolExecDelta attribution.
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
	// Shared provider for both parent and child.
	client := ollama.NewClient(ollama.Config{Host: "http://localhost:11434", Model: "llama3.2"})
	adapter := ollama.New(ollama.Config{Client: client})

	// Mock search tool for the researcher sub-agent.
	searchTool := &types.ToolFunc{
		Def: types.ToolDef{
			Name:        "search",
			Description: "Search the web for information on a topic",
			Parameters: types.ParameterSchema{
				Type:     "object",
				Required: []string{"query"},
				Properties: map[string]types.PropertyDef{
					"query": {Type: "string", Description: "Search query"},
				},
			},
		},
		Fn: func(ctx context.Context, args map[string]any) (string, error) {
			query, _ := args["query"].(string)
			return fmt.Sprintf("Results for %q: Go 1.24 adds generic type aliases, "+
				"improved range-over-func iterators, and Swiss Tables map implementation.", query), nil
		},
	}

	// Build the parent agent with a researcher sub-agent.
	agent := agentsdk.New(agentsdk.Config{
		Name:         "coordinator",
		SystemPrompt: "You coordinate research tasks. Delegate research to the researcher.",
		Provider:     adapter,
		SubAgents: []agentsdk.SubAgentDef{
			{
				Name:         "researcher",
				Description:  "A research specialist that can search for information",
				SystemPrompt: "You are a research assistant. Use the search tool to find information.",
				Provider:     adapter,
				Tools:        types.NewToolRegistry(searchTool),
			},
		},
	})

	// Invoke with a research request.
	stream := agent.Invoke(context.Background(), []types.Message{
		types.UserMsg(types.Text("Research the latest Go features")),
	})

	// Consume deltas, showing sub-agent attribution.
	for delta := range stream.Deltas() {
		switch d := delta.(type) {
		case types.PartDelta:
			fmt.Print(d.Text)
		case types.ToolExecStartDelta:
			fmt.Printf("\n[tool-start] %s (id=%s)\n", d.Name, d.ToolCallID)
		case types.ToolExecDelta:
			if inner, ok := d.Inner.(types.PartDelta); ok {
				fmt.Printf("  [sub-agent %s] %s", d.ToolCallID, inner.Text)
			}
		case types.ToolExecEndDelta:
			fmt.Printf("\n[tool-end] id=%s\n", d.ToolCallID)
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
