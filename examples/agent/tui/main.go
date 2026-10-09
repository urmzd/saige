// Package main demonstrates TUI modes with a coordinator agent delegating
// to a researcher sub-agent, including the multi-turn Runner pattern.
//
// Usage:
//
//	go run ./examples/tui/              # non-interactive (verbose) single-turn
//	go run ./examples/tui/ -interactive # interactive bubbletea single-turn
//	go run ./examples/tui/ -runner      # multi-turn Runner (interactive)
//	go run ./examples/tui/ -runner -verbose # multi-turn Runner (verbose)
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"

	tea "github.com/charmbracelet/bubbletea"
	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/provider/ollama"
	"github.com/urmzd/saige/agent/tui"
	"github.com/urmzd/saige/agent/types"
)

func main() {
	interactive := flag.Bool("interactive", false, "use interactive bubbletea TUI (requires TTY)")
	runner := flag.Bool("runner", false, "use multi-turn Runner pattern")
	verbose := flag.Bool("verbose", false, "use verbose mode with Runner")
	flag.Parse()

	client := ollama.NewClient("http://localhost:11434", "qwen3.5:4b", "")
	if *interactive || (*runner && !*verbose) {
		client.Logger = log.New(io.Discard, "", 0)
	}
	adapter := ollama.NewAdapter(client)

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

	agent := agentsdk.NewAgent(agentsdk.AgentConfig{
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

	// Multi-turn Runner pattern: user types messages in a loop.
	if *runner {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()

		r := &tui.Runner{
			Title:   "Research Agent",
			Verbose: *verbose,
		}
		if err := agentsdk.Run(ctx, agent, r); err != nil {
			log.Fatal(err)
		}
		return
	}

	// Single-turn: invoke once and display results.
	stream := agent.Invoke(context.Background(), []types.Message{
		types.NewUserMessage("Research the latest Go features"),
	})

	if *interactive {
		runInteractive(agent, stream)
	} else {
		runVerbose(agent, stream)
	}
}

func agentHeader(agent *agentsdk.Agent) tui.AgentHeader {
	info := agent.Info()
	h := tui.AgentHeader{
		Name:      info.Name,
		Provider:  info.Provider,
		Tools:     info.Tools,
		SubAgents: info.SubAgents,
	}
	tui.PopulateEnv(&h)
	return h
}

func runInteractive(agent *agentsdk.Agent, stream *agentsdk.EventStream) {
	// WithCancel stops the run when the user quits before it finishes.
	model := tui.NewStreamModel(agentHeader(agent), stream.Deltas()).WithCancel(stream.Cancel)
	p := tea.NewProgram(model)

	finalModel, err := p.Run()
	if err != nil {
		log.Fatalf("TUI error: %v", err)
	}

	m := finalModel.(tui.StreamModel)
	if m.Err() != nil {
		log.Fatalf("Stream error: %v", m.Err())
	}

	fmt.Print(tui.RenderReport("Final Report", m.FinalReport()))
}

func runVerbose(agent *agentsdk.Agent, stream *agentsdk.EventStream) {
	result := tui.StreamVerbose(agentHeader(agent), stream.Deltas(), nil)
	if result.Err != nil {
		log.Fatalf("Stream error: %v", result.Err)
	}

	fmt.Print(tui.RenderReport("Final Report", result.Text))
}
