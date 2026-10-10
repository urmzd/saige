package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/urmzd/saige/tools"
	"github.com/urmzd/saige/tools/exec"
)

// Values of --tools besides a group list.
const (
	toolsNone     = "none"
	toolsReadOnly = "readonly"
	toolsHarness  = "harness"
)

// harnessFlags selects the built-in harness toolset for ask and chat.
type harnessFlags struct {
	mode      string
	workspace string
	sandbox   string
	network   string
}

// addHarnessFlags registers --tools and its companions. def is the --tools
// default for the command.
func addHarnessFlags(cmd *cobra.Command, h *harnessFlags, def string) {
	f := cmd.Flags()
	f.StringVar(&h.mode, "tools", def, "Built-in tools: none, readonly (read_file, list_dir, glob, grep), harness (adds write_file, edit_file, execute_code, fetch_url), or groups such as read,exec")
	f.StringVar(&h.workspace, "workspace", ".", "Directory the built-in tools are confined to")
	f.StringVar(&h.sandbox, "sandbox", string(tools.SandboxSubprocess), "Sandbox for execute_code: subprocess or docker")
	f.StringVar(&h.network, "exec-network", string(exec.NetworkDeny), "Network policy for execute_code: deny (needs an isolating sandbox) or allow")
}

// groups maps --tools to harness groups; nil means no built-in tools.
func (h harnessFlags) groups() ([]tools.Group, error) {
	switch strings.TrimSpace(h.mode) {
	case "", toolsNone:
		return nil, nil
	case toolsReadOnly:
		return tools.ReadOnly(), nil
	case toolsHarness:
		return tools.AllGroups(), nil
	}
	g, err := tools.ParseGroups(h.mode)
	if err != nil {
		return nil, fmt.Errorf("--tools: %w", err)
	}
	return g, nil
}

// build returns the harness toolset, or nil when --tools is none. Write and
// exec tools carry approval markers: chat asks at the prompt, and ask
// answers with --approve.
func (h harnessFlags) build(ctx context.Context) (*tools.Toolset, error) {
	groups, err := h.groups()
	if err != nil || len(groups) == 0 {
		return nil, err
	}
	set, err := tools.Harness(ctx, tools.HarnessOptions{
		Root:        h.workspace,
		Groups:      groups,
		SandboxKind: tools.SandboxKind(h.sandbox),
		Network:     exec.NetworkPolicy(h.network),
	})
	if err != nil {
		return nil, fmt.Errorf("--tools %s: %w", h.mode, err)
	}
	return set, nil
}
