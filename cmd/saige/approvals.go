package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/cmd/internal/approvals"
)

func newApprovalsCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "approvals",
		Short: "List and decide approvals a saige agent holds for a client that cannot ask you",
		Long: `When saige-mcp runs an agent for an MCP client without elicitation (such as
opencode or Zed), a call that needs your approval is held instead of refused.
The client's model shows you a token; decide it here, then let the model
resume:

  saige approvals list
  saige approvals approve apr_... [--grant tool]
  saige approvals deny apr_... [--message "use the staging bucket"]

A grant (once, args, tool or session) covers later calls of the same agent
run, within the widest scope the agent's definition allows. Held approvals
live in ` + "$" + approvals.EnvDir + `, else ~/.local/state/saige/approvals.`,
	}
	cmd.PersistentFlags().StringVar(&dir, "dir", "", "Approvals directory (default $"+approvals.EnvDir+" or ~/.local/state/saige/approvals)")
	open := func() (*approvals.Store, error) {
		d := dir
		if d == "" {
			d = approvals.DefaultDir(os.Getenv)
		}
		return approvals.Open(d)
	}
	cmd.AddCommand(newApprovalsListCmd(open), newApprovalsDecideCmd(open, true), newApprovalsDecideCmd(open, false))
	return cmd
}

func newApprovalsListCmd(open func() (*approvals.Store, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List held approvals, oldest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := open()
			if err != nil {
				return err
			}
			entries, err := store.List()
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if persistentFlagVars.isJSON() {
				if entries == nil {
					entries = []approvals.Entry{}
				}
				return writeJSONTo(w, entries)
			}
			if len(entries) == 0 {
				_, err := fmt.Fprintln(w, "no held approvals")
				return err
			}
			tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "TOKEN\tAGENT\tTOOL\tARGUMENTS\tSTATE")
			now := time.Now()
			for _, e := range entries {
				state := "pending, expires " + e.ExpiresAt.Local().Format(time.Kitchen)
				switch {
				case e.Decision != nil && e.Decision.Approved:
					state = "approved"
				case e.Decision != nil:
					state = "denied"
				case now.After(e.ExpiresAt):
					state = "expired"
				}
				args, _ := jsonCompact(e.Arguments)
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", e.Token, e.Agent, e.Tool, oneLine(args, 60), state)
			}
			return tw.Flush()
		},
	}
}

func newApprovalsDecideCmd(open func() (*approvals.Store, error), approve bool) *cobra.Command {
	var (
		grant   string
		message string
	)
	use, short := "deny TOKEN", "Deny a held call; the agent sees the refusal and the message"
	if approve {
		use, short = "approve TOKEN", "Approve a held call, optionally granting later calls"
	}
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := open()
			if err != nil {
				return err
			}
			d := approvals.Decision{Approved: approve, Message: message, Approver: approver()}
			if grant != "" && grant != string(types.GrantOnce) {
				d.Grant = &types.GrantRequest{Scope: types.GrantScope(grant)}
			}
			if err := store.Decide(args[0], d); err != nil {
				return invalidInput(err)
			}
			verdict := "denied"
			if approve {
				verdict = "approved"
			}
			if persistentFlagVars.isJSON() {
				return writeJSONTo(cmd.OutOrStdout(), map[string]any{"token": args[0], "approved": approve})
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s %s; the agent continues when the model resumes it\n", args[0], verdict)
			return err
		},
	}
	if approve {
		cmd.Flags().StringVar(&grant, "grant", "", "Also approve later calls of this run: once (default), tool or session; destructive tools always ask")
	}
	cmd.Flags().StringVar(&message, "message", "", "Message the agent sees with the decision")
	return cmd
}

// approver names the person deciding: the OS user.
func approver() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}

func jsonCompact(v any) (string, error) {
	raw, err := json.Marshal(v)
	return string(raw), err
}
