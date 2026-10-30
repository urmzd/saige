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

func newAgentRunCmd(ctx context.Context) *cobra.Command {
	var tmplName, approve, agentRef, sessionID, sessionsDir string
	var trace bool
	var hf harnessFlags

	cmd := &cobra.Command{
		Use:   "run NAME [question]",
		Short: "Run one turn; use NAME - for an ad hoc agent, --session to continue",
		RunE: func(cmd *cobra.Command, args []string) error {
			cf := persistentFlagVars

			if approve != approveDeny && approve != approveAllow {
				return fmt.Errorf("--approve must be %q or %q, got %q", approveDeny, approveAllow, approve)
			}

			if agentRef == "" {
				if len(args) == 0 {
					return errors.New("usage: saige agent run NAME [question] (use - for an ad hoc agent)")
				}
				agentRef, args = args[0], args[1:]
			}
			if agentRef == "-" {
				agentRef = ""
			}
			question := strings.Join(args, " ")
			if question == "" {
				question = readStdin()
			}
			if question == "" {
				return errors.New(`usage: saige agent run NAME [flags] "question"`)
			}

			tmpl := tui.TemplateByName(tmplName)
			if tmplName == "minimal" {
				tmpl.ShowMarkers = false
			}
			out := tui.ResolveOutputWriters(cf.isJSON(), tmpl, cmd.OutOrStdout(), cmd.ErrOrStderr())
			opts := agentRunOptions{sessionID: sessionID, sessionsDir: sessionsDir, trace: trace, diagnostics: cmd.ErrOrStderr()}

			if agentRef != "" {
				run, err := bindCLIAgent(ctx, cmd, cf, agentRef, hf.options(), false)
				if err != nil {
					return reported(out, err)
				}
				defer run.cleanup()
				a, err := run.bound.NewAgent()
				if err != nil {
					return reported(out, err)
				}
				opts.identity = run.bound.Resolved.Digest
				opts.definition = run.bound.Resolved.Name + "@" + run.bound.Resolved.Version
				if err := runAgentTurn(ctx, a, question, out, approve == approveAllow, opts); err != nil {
					return reported(out, err)
				}
				if !cf.isJSON() {
					fmt.Fprintln(cmd.OutOrStdout())
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

			agentCfg := agentsdk.Config{
				Name:         cliName,
				SystemPrompt: *cf.system,
			}
			if len(tools) > 0 {
				agentCfg.Tools = types.NewToolRegistry(tools...)
			}

			a, err := agentsdk.New(agentCfg, agentsdk.WithPreset(bundle), agentsdk.WithToolset(harness))
			if err != nil {
				return reported(out, err)
			}
			opts.identity = "adhoc"
			if err := runAgentTurn(ctx, a, question, out, approve == approveAllow, opts); err != nil {
				return reported(out, err)
			}
			if !cf.isJSON() {
				// JSON lines already end in a newline; text output does not.
				fmt.Fprintln(cmd.OutOrStdout())
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&tmplName, "template", "minimal", "Output template (default|minimal|detailed)")
	cmd.Flags().StringVar(&approve, "approve", approveDeny, "Decision for tool calls that need approval, since agent run cannot prompt (deny|allow)")
	addHarnessFlags(cmd, &hf, toolsNone)
	cmd.Flags().StringVar(&agentRef, "agent", "", "Run an agent definition: NAME or NAME@RANGE (see saige agent list)")

	cmd.Flags().StringVar(&sessionID, "session", "", "Create or continue a named session (letters, digits, hyphens and underscores)")
	cmd.Flags().StringVar(&sessionsDir, "sessions-dir", "", "Session directory (default: user config directory/saige/sessions)")
	cmd.Flags().BoolVar(&trace, "trace", false, "Write load, session, provider and tool events as JSON lines to stderr")
	return cmd
}

// runAsk invokes agent once and renders the response on out. Every marker is
// resolved inline with allow, so a marked tool call can never leave the
// command waiting for an answer nobody will give.
func runAsk(ctx context.Context, agent *agentsdk.Agent, question string, out tui.Output, allow bool) error {
	return runAskTraced(ctx, agent, question, out, allow, agentRunOptions{})
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
