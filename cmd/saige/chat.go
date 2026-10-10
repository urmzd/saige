package main

import (
	"context"

	"github.com/spf13/cobra"
	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/tui"
	"github.com/urmzd/saige/agent/types"
)

func newChatCmd(ctx context.Context) *cobra.Command {
	var verbose, noAnimation bool
	var tmplName, agentRef string
	var hf harnessFlags

	cmd := &cobra.Command{
		Use:   "chat",
		Short: "Interactive multi-turn chat session",
		RunE: func(cmd *cobra.Command, args []string) error {
			cf := persistentFlagVars

			tmpl := tui.TemplateByName(tmplName)
			out := tui.ResolveOutput(cf.isJSON(), tmpl)

			if agentRef != "" {
				run, err := bindCLIAgent(ctx, cmd, cf, agentRef, hf.options(), verbose)
				if err != nil {
					return reported(out, err)
				}
				defer run.cleanup()
				runner := &tui.Runner{Title: run.bound.Resolved.Name, Verbose: verbose, Template: tmpl, Output: out, NoAnimation: noAnimation}
				return agentsdk.Run(ctx, run.bound.NewAgent(), runner)
			}

			bundle, err := resolveBundle(ctx, cf, verbose)
			if err != nil {
				return reported(out, err)
			}

			tools, cleanup, err := buildTools(ctx, cf)
			if err != nil {
				return reported(out, err)
			}
			defer cleanup()

			harness, err := hf.build(ctx)
			if err != nil {
				return reported(out, err)
			}

			agentCfg := agentsdk.AgentConfig{
				Name:         cliName,
				SystemPrompt: *cf.system,
			}
			if len(tools) > 0 {
				agentCfg.Tools = types.NewToolRegistry(tools...)
			}

			agent := agentsdk.NewAgent(agentCfg, agentsdk.WithPreset(bundle), agentsdk.WithToolset(harness))

			runner := &tui.Runner{
				Title:       cliName,
				Verbose:     verbose,
				Template:    tmpl,
				Output:      out,
				NoAnimation: noAnimation,
			}

			return agentsdk.Run(ctx, agent, runner)
		},
	}

	cmd.Flags().BoolVar(&verbose, "verbose", false, "Use plain-text streaming instead of interactive TUI")
	cmd.Flags().BoolVar(&noAnimation, "no-animation", false, "Turn off the spinner, fade-in and smooth scrolling (also off with NO_COLOR or SAIGE_REDUCED_MOTION)")
	cmd.Flags().StringVar(&tmplName, "template", "default", "Output template (default|minimal|detailed)")
	addHarnessFlags(cmd, &hf, toolsReadOnly)
	cmd.Flags().StringVar(&agentRef, "agent", "", "Chat with an agent definition: NAME or NAME@RANGE (see saige agent list)")

	return cmd
}
