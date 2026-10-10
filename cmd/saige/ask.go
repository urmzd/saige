package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/tui"
	"github.com/urmzd/saige/agent/types"
)

// Approval policies for tool calls that carry an approval marker in
// non-interactive commands, where nobody is there to answer a prompt.
const (
	approveDeny  = "deny"
	approveAllow = "allow"
)

// deniedByFlag is the reason the model sees when --approve=deny rejects a call.
const deniedByFlag = "denied by --approve=deny: this session cannot approve tool calls that need confirmation"

func newAskCmd(ctx context.Context) *cobra.Command {
	var tmplName, approve, agentRef string
	var hf harnessFlags

	cmd := &cobra.Command{
		Use:   "ask [question]",
		Short: "Single-shot question (pipe-friendly)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cf := persistentFlagVars

			if approve != approveDeny && approve != approveAllow {
				return fmt.Errorf("--approve must be %q or %q, got %q", approveDeny, approveAllow, approve)
			}

			question := strings.Join(args, " ")
			if question == "" {
				question = readStdin()
			}
			if question == "" {
				return errors.New(`usage: saige ask [flags] "question"`)
			}

			out := tui.ResolveOutput(cf.isJSON(), tui.TemplateByName(tmplName))

			if agentRef != "" {
				run, err := bindCLIAgent(ctx, cmd, cf, agentRef, hf.options(), false)
				if err != nil {
					return reported(out, err)
				}
				defer run.cleanup()
				if err := runAsk(ctx, run.bound.NewAgent(), question, out, approve == approveAllow); err != nil {
					return reported(out, err)
				}
				if !cf.isJSON() {
					fmt.Println()
				}
				return nil
			}

			bundle, err := resolveBundle(ctx, cf, false)
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

			if err := runAsk(ctx, agentsdk.NewAgent(agentCfg, agentsdk.WithPreset(bundle), agentsdk.WithToolset(harness)), question, out, approve == approveAllow); err != nil {
				return reported(out, err)
			}
			if !cf.isJSON() {
				// JSON lines already end in a newline; text output does not.
				fmt.Println()
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&tmplName, "template", "default", "Output template (default|minimal|detailed)")
	cmd.Flags().StringVar(&approve, "approve", approveDeny, "Decision for tool calls that need approval, since ask cannot prompt (deny|allow)")
	addHarnessFlags(cmd, &hf, toolsNone)
	cmd.Flags().StringVar(&agentRef, "agent", "", "Run an agent definition: NAME or NAME@RANGE (see saige agent list)")

	return cmd
}

// runAsk invokes agent once and renders the response on out. Every marker is
// resolved inline with allow, so a marked tool call can never leave the
// command waiting for an answer nobody will give.
func runAsk(ctx context.Context, agent *agentsdk.Agent, question string, out tui.Output, allow bool) error {
	stream := agent.Invoke(ctx, []types.Message{types.UserMsg(types.Text(question))})
	resolve := func(d types.MarkerDelta) {
		if allow {
			stream.ResolveMarker(d.ToolCallID, true, nil)
			return
		}
		stream.ResolveMarkerWithMessage(d.ToolCallID, false, nil, deniedByFlag)
	}
	result := tui.StreamDeltasResolving(out, tui.AgentHeader{}, stream.Deltas(), resolve)
	return result.Err
}

func readStdin() string {
	info, err := os.Stdin.Stat()
	if err != nil {
		return ""
	}
	if info.Mode()&os.ModeCharDevice != 0 {
		return ""
	}
	return readAll(os.Stdin)
}

func readAll(r io.Reader) string {
	var lines []string
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
