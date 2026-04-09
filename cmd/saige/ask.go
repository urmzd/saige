package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/tui"
	"github.com/urmzd/saige/agent/types"
)

func newAskCmd(ctx context.Context) *cobra.Command {
	var raw bool
	var tmplName string

	cmd := &cobra.Command{
		Use:   "ask [question]",
		Short: "Single-shot question (pipe-friendly)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cf := persistentFlagVars

			question := strings.Join(args, " ")
			if question == "" {
				question = readStdin()
			}
			if question == "" {
				fmt.Fprintln(os.Stderr, "usage: saige ask [flags] \"question\"")
				os.Exit(1)
			}

			out := tui.ResolveOutput(raw, tui.TemplateByName(tmplName))

			provider, err := resolveProvider(ctx, cf, false)
			if err != nil {
				out.Error(err)
				os.Exit(1)
			}

			tools, cleanup, err := buildTools(ctx, cf)
			if err != nil {
				out.Error(err)
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

			result := out.StreamDeltas(tui.AgentHeader{}, stream.Deltas())
			if result.Err != nil {
				out.Error(result.Err)
				os.Exit(1)
			}
			fmt.Println()
			return nil
		},
	}

	cmd.Flags().BoolVar(&raw, "raw", false, "Output raw text only (no styling), useful for pipes")
	cmd.Flags().StringVar(&tmplName, "template", "default", "Output template (default|minimal|detailed)")

	return cmd
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
