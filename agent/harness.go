package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/tools"
)

// WithHarnessTools adds the curated harness toolset from tools.Harness:
// read_file, list_dir, glob, and grep by default, and write_file,
// edit_file, execute_code, and fetch_url when their groups are enabled,
// plus the scratch tools. An agent has no tools unless an option like this
// one adds them.
//
// Large results spill to the toolset's workspace, which also becomes the
// agent's workspace unless WithWorkspace sets one; the scratch tools and the
// spill always use the workspace attached to the call. write_file,
// edit_file, and execute_code carry approval markers, so a run pauses for a
// decision before a file changes or code runs. Answer with a
// Resolution.Grant under WithApprovalPolicy to stop asking for the rest of
// the conversation; a grant never covers execute_code on a sandbox that does
// not confine file changes, because it is destructive-class there.
//
// A toolset that cannot be built, for example because Root is missing or
// Docker is not running, fails every run with the error. Call tools.Harness
// and WithToolset to handle the error when the agent is built instead.
//
// To disclose the tools on demand, pin the core ones with
// selector.DeferredTools:
//
//	set, err := tools.Harness(ctx, opts)
//	deferred := selector.NewDeferredTools(set.Core()...)
//	a := agent.New(cfg, agent.WithToolset(set),
//		agent.WithToolPolicy(deferred), agent.WithTools(deferred.Tool()))
func WithHarnessTools(opts tools.HarnessOptions) Option {
	return func(c *Config) {
		set, err := tools.Harness(context.Background(), opts)
		if err != nil {
			c.optionErr = errors.Join(c.optionErr, fmt.Errorf("agent: harness tools: %w", err))
			return
		}
		WithToolset(set)(c)
	}
}

// WithToolset adds a toolset built with tools.Harness, and uses its
// workspace when the agent has none.
func WithToolset(set *tools.Toolset) Option {
	return func(c *Config) {
		if set == nil {
			return
		}
		WithTools(set.Tools...)(c)
		if c.Workspace == nil {
			c.Workspace = set.Workspace
		}
	}
}

// WithTools adds tools to the agent's registry. A tool with the name of one
// already registered replaces it. The registry in Config.Tools is
// copied, not changed.
func WithTools(ts ...types.Tool) Option {
	return func(c *Config) {
		var existing []types.Tool
		if c.Tools != nil {
			existing = c.Tools.All()
		}
		c.Tools = types.NewToolRegistry(append(existing, ts...)...)
	}
}
