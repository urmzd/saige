package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/tui"
	"github.com/urmzd/saige/agent/types"
)

func runChat(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("chat", flag.ExitOnError)
	cf := addCommonFlags(fs)
	verbose := fs.Bool("verbose", false, "Use plain-text streaming instead of interactive TUI")
	tmplName := fs.String("template", "default", "Output template (default|minimal|detailed)")
	_ = fs.Parse(args)

	tmpl := tui.TemplateByName(*tmplName)
	out := tui.ResolveOutput(false, tmpl)

	provider, err := resolveProvider(ctx, cf, *verbose)
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
		Verbose:  *verbose,
		Template: tmpl,
		Output:   out,
	}

	if err := agentsdk.Run(ctx, agent, runner); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
