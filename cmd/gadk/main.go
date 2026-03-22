// Command gadk is a development CLI for the graph-agent-dev-kit.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/urmzd/graph-agent-dev-kit/agent"
	"github.com/urmzd/graph-agent-dev-kit/agent/core"
	"github.com/urmzd/graph-agent-dev-kit/agent/provider/ollama"
)

const version = "0.3.0"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "agent":
		runAgent(os.Args[2:])
	case "version":
		fmt.Printf("gadk v%s\n", version)
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `Usage: gadk <command> [flags]

Commands:
  agent    Run an interactive agent session
  version  Print version info
  help     Show this help`)
}

func runAgent(args []string) {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	host := fs.String("host", "http://localhost:11434", "Ollama host URL")
	model := fs.String("model", "qwen3.5:4b", "Model name")
	system := fs.String("system", "You are a helpful assistant.", "System prompt")
	fs.Parse(args)

	client := ollama.NewClient(*host, *model, "nomic-embed-text")
	a := agent.NewAgent(agent.AgentConfig{
		Name:         "gadk-agent",
		SystemPrompt: *system,
		Provider:     ollama.NewAdapter(client),
	})

	ctx := context.Background()
	scanner := bufio.NewScanner(os.Stdin)

	fmt.Fprintf(os.Stderr, "gadk agent (model=%s)\nType your message, Ctrl+D to exit.\n\n", *model)

	for {
		fmt.Fprint(os.Stderr, "> ")
		if !scanner.Scan() {
			break
		}
		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}

		stream := a.Invoke(ctx, []core.Message{core.NewUserMessage(input)})
		for delta := range stream.Deltas() {
			switch d := delta.(type) {
			case core.TextContentDelta:
				fmt.Print(d.Content)
			case core.ErrorDelta:
				fmt.Fprintf(os.Stderr, "\nerror: %v\n", d.Error)
			case core.DoneDelta:
				fmt.Println()
			}
		}
	}
}
