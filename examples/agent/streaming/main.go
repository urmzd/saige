// Package main demonstrates consuming all delta types from the agent event
// stream. Each delta type is type-switched and printed with ANSI color codes
// to make it easy to distinguish text, tool, usage, and error deltas in the
// terminal.
package main

import (
	"context"
	"fmt"
	"log"

	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/provider/ollama"
	"github.com/urmzd/saige/agent/types"
)

// ANSI color codes for terminal output.
const (
	colorReset   = "\033[0m"
	colorGreen   = "\033[32m" // text content
	colorYellow  = "\033[33m" // tool call deltas
	colorCyan    = "\033[36m" // tool execution deltas
	colorMagenta = "\033[35m" // usage/metadata
	colorRed     = "\033[31m" // errors
	colorDim     = "\033[2m"  // structural markers
)

func main() {
	client := ollama.NewClient("http://localhost:11434", "llama3.2", "")
	adapter := ollama.NewAdapter(client)

	addTool := &types.ToolFunc{
		Def: types.ToolDef{
			Name:        "add",
			Description: "Add two numbers",
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
		Name:         "streaming-demo",
		SystemPrompt: "You are a helpful calculator. Use the add tool when asked to add numbers.",
		Provider:     adapter,
		Tools:        types.NewToolRegistry(addTool),
	})

	stream := agent.Invoke(context.Background(), []types.Message{
		types.NewUserMessage("What is 10 + 25? Please use the tool."),
	})

	for delta := range stream.Deltas() {
		switch d := delta.(type) {
		case types.TextStartDelta:
			fmt.Printf("%s[text-start]%s ", colorDim, colorReset)
		case types.TextContentDelta:
			fmt.Printf("%s%s%s", colorGreen, d.Content, colorReset)
		case types.TextEndDelta:
			fmt.Printf(" %s[text-end]%s\n", colorDim, colorReset)
		case types.ToolCallStartDelta:
			fmt.Printf("%s[tool-call-start] name=%s id=%s%s\n", colorYellow, d.Name, d.ID, colorReset)
		case types.ToolCallArgumentDelta:
			fmt.Printf("%s  args: %s%s\n", colorYellow, d.Content, colorReset)
		case types.ToolCallEndDelta:
			fmt.Printf("%s[tool-call-end] args=%v%s\n", colorYellow, d.Arguments, colorReset)
		case types.ToolExecStartDelta:
			fmt.Printf("%s[exec-start] %s (id=%s)%s\n", colorCyan, d.Name, d.ToolCallID, colorReset)
		case types.ToolExecDelta:
			fmt.Printf("%s  [exec-delta] id=%s inner=%T%s\n", colorCyan, d.ToolCallID, d.Inner, colorReset)
		case types.ToolExecEndDelta:
			fmt.Printf("%s[exec-end] id=%s result=%s err=%s%s\n", colorCyan, d.ToolCallID, d.Result, d.Error, colorReset)
		case types.UsageDelta:
			fmt.Printf("%s[usage] prompt=%d completion=%d total=%d latency=%s%s\n",
				colorMagenta, d.PromptTokens, d.CompletionTokens, d.TotalTokens, d.Latency, colorReset)
		case types.ErrorDelta:
			fmt.Printf("%s[error] %v%s\n", colorRed, d.Error, colorReset)
			log.Fatal(d.Error)
		case types.DoneDelta:
			fmt.Printf("%s[done]%s\n", colorDim, colorReset)
		}
	}

	if err := stream.Wait(); err != nil {
		log.Fatal(err)
	}
}
