package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/tui"
	"github.com/urmzd/saige/agent/types"
)

func runAsk(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("ask", flag.ExitOnError)
	cf := addCommonFlags(fs)
	raw := fs.Bool("raw", false, "Output raw text only (no styling), useful for pipes")
	_ = fs.Parse(args)

	question := strings.Join(fs.Args(), " ")
	if question == "" {
		question = readStdin()
	}
	if question == "" {
		fmt.Fprintln(os.Stderr, "usage: saige ask [flags] \"question\"")
		os.Exit(1)
	}

	provider, err := resolveProvider(ctx, cf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	tools, cleanup, err := buildTools(ctx, cf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer cleanup()

	agentCfg := agentsdk.AgentConfig{
		Name:         "saige",
		SystemPrompt: *cf.system,
		Provider:     provider,
	}
	if len(tools) > 0 {
		agentCfg.Tools = types.NewToolRegistry(tools...)
	}

	agent := agentsdk.NewAgent(agentCfg)
	stream := agent.Invoke(ctx, []types.Message{types.NewUserMessage(question)})

	if *raw {
		for delta := range stream.Deltas() {
			switch d := delta.(type) {
			case types.TextContentDelta:
				fmt.Print(d.Content)
			case types.ErrorDelta:
				fmt.Fprintf(os.Stderr, "error: %v\n", d.Error)
				os.Exit(1)
			}
		}
		fmt.Println()
	} else {
		result := tui.StreamVerbose(tui.AgentHeader{}, stream.Deltas(), os.Stdout)
		if result.Err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", result.Err)
			os.Exit(1)
		}
		fmt.Println()
	}
}

func readStdin() string {
	info, err := os.Stdin.Stat()
	if err != nil {
		return ""
	}
	if info.Mode()&os.ModeCharDevice != 0 {
		return ""
	}
	var lines []string
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
