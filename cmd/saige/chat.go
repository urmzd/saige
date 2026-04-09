package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/tui"
	"github.com/urmzd/saige/agent/types"
)

func newChatCmd(ctx context.Context) *cobra.Command {
	var verbose bool
	var tmplName string

	cmd := &cobra.Command{
		Use:   "chat",
		Short: "Interactive multi-turn chat session",
		RunE: func(cmd *cobra.Command, args []string) error {
			cf := persistentFlagVars

			tmpl := tui.TemplateByName(tmplName)
			out := tui.ResolveOutput(false, tmpl)

			provider, err := resolveProvider(ctx, cf, verbose)
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

			runner := &tui.Runner{
				Title:    "saige",
				Verbose:  verbose,
				Template: tmpl,
				Output:   out,
			}

			if err := agentsdk.Run(ctx, agent, runner); err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				os.Exit(1)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&verbose, "verbose", false, "Use plain-text streaming instead of interactive TUI")
	cmd.Flags().StringVar(&tmplName, "template", "default", "Output template (default|minimal|detailed)")

	return cmd
}
