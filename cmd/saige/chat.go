package main

import (
	"context"

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
			out := tui.ResolveOutput(cf.isJSON(), tmpl)

			bundle, err := resolveBundle(ctx, cf, verbose)
			if err != nil {
				return reported(out, err)
			}

			tools, cleanup, err := buildTools(ctx, cf)
			if err != nil {
				return reported(out, err)
			}
			defer cleanup()

			agentCfg := agentsdk.AgentConfig{
				Name:         cliName,
				SystemPrompt: *cf.system,
			}
			if len(tools) > 0 {
				agentCfg.Tools = types.NewToolRegistry(tools...)
			}

			agent := agentsdk.NewAgent(agentCfg, agentsdk.WithPreset(bundle))

			runner := &tui.Runner{
				Title:    cliName,
				Verbose:  verbose,
				Template: tmpl,
				Output:   out,
			}

			return agentsdk.Run(ctx, agent, runner)
		},
	}

	cmd.Flags().BoolVar(&verbose, "verbose", false, "Use plain-text streaming instead of interactive TUI")
	cmd.Flags().StringVar(&tmplName, "template", "default", "Output template (default|minimal|detailed)")

	return cmd
}
