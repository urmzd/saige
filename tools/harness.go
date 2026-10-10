// Package tools assembles the packs under it into one curated, safe by
// default toolset for an agent harness: file reading and search, file
// changes, code execution, URL fetch, and the run's scratch workspace.
//
// Harness returns the tools for the groups asked for, under these defaults:
//
//   - Only the read group is enabled unless Groups says otherwise.
//   - Every path is confined to Root.
//   - write_file, edit_file, and execute_code carry a human_approval marker,
//     so the agent loop asks before a file changes or code runs.
//   - execute_code runs behind an exec.Sandbox with the network denied, a
//     scrubbed environment, and a time limit.
//   - fetch_url refuses private, local, and cloud metadata addresses.
//   - Results larger than the spill threshold are stored in the workspace
//     and replaced by a preview the model can page through with
//     scratch_read.
//
// The packs stay usable on their own; see tools/fs, tools/exec, and
// tools/fetch.
package tools

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/agent/workspace"
	"github.com/urmzd/saige/tools/exec"
	"github.com/urmzd/saige/tools/fetch"
	"github.com/urmzd/saige/tools/fs"
)

// Group is a set of harness tools enabled together.
type Group string

const (
	// GroupRead adds read_file, list_dir, glob, and grep.
	GroupRead Group = "read"
	// GroupWrite adds write_file and edit_file, both approval-marked.
	GroupWrite Group = "write"
	// GroupExec adds execute_code, approval-marked and sandboxed.
	GroupExec Group = "exec"
	// GroupWeb adds fetch_url.
	GroupWeb Group = "web"
)

// ReadOnly is the default group set: reading and searching the workspace.
func ReadOnly() []Group { return []Group{GroupRead} }

// AllGroups enables every group.
func AllGroups() []Group { return []Group{GroupRead, GroupWrite, GroupExec, GroupWeb} }

// ParseGroups reads a comma-separated group list such as "read,exec".
func ParseGroups(s string) ([]Group, error) {
	var out []Group
	for _, part := range strings.Split(s, ",") {
		g := Group(strings.TrimSpace(part))
		switch g {
		case "":
			continue
		case GroupRead, GroupWrite, GroupExec, GroupWeb:
			if !slices.Contains(out, g) {
				out = append(out, g)
			}
		default:
			return nil, fmt.Errorf("tools: unknown group %q (want read, write, exec, or web)", g)
		}
	}
	return out, nil
}

// Tool names in the harness toolset.
const (
	ReadFileName    = "read_file"
	ListDirName     = "list_dir"
	GlobName        = "glob"
	GrepName        = "grep"
	WriteFileName   = "write_file"
	EditFileName    = "edit_file"
	ExecuteCodeName = exec.CodeToolName
	FetchURLName    = "fetch_url"
)

// SandboxKind names a sandbox Harness builds when HarnessOptions.Sandbox is
// nil.
type SandboxKind string

const (
	// SandboxSubprocess runs code as a local child process, under the
	// platform's network-denying wrapper when the network is denied. It
	// does not confine file changes to the workspace, so execute_code is
	// destructive-class on it. It is the default.
	SandboxSubprocess SandboxKind = "subprocess"
	// SandboxDocker runs code in a container with only the workspace
	// mounted, so execute_code is write-class on it.
	SandboxDocker SandboxKind = "docker"
)

// DefaultMaxOutputBytes is how much stdout and stderr, each, a sandbox
// Harness builds keeps. Output above the spill threshold reaches the model
// as a preview, so this cap only bounds memory and workspace size.
const DefaultMaxOutputBytes = 1 << 20

// HarnessOptions configures Harness. Only Root is required.
type HarnessOptions struct {
	// Root is the workspace directory every tool is confined to.
	Root string
	// Groups lists the enabled groups. Empty means ReadOnly.
	Groups []Group

	// Sandbox runs execute_code. nil builds one of SandboxKind.
	Sandbox exec.Sandbox
	// SandboxKind picks the sandbox built when Sandbox is nil. Empty means
	// SandboxSubprocess.
	SandboxKind SandboxKind
	// DockerImage is the image for SandboxDocker. Empty means
	// exec.DefaultDockerImage.
	DockerImage string
	// Network is the network rule for execute_code. Empty means
	// exec.NetworkDeny, which needs a sandbox that blocks the network.
	Network exec.NetworkPolicy
	// Policy replaces exec.DefaultPolicy for execute_code. Network, Timeout,
	// and MaxTimeout below override its fields when set.
	Policy *exec.Policy
	// Timeout is the default time limit of one execute_code call.
	Timeout time.Duration
	// MaxTimeout caps the time limit a call may ask for.
	MaxTimeout time.Duration
	// Languages limits the languages execute_code offers. Empty offers
	// every language the sandbox has among shell, python, and go.
	Languages []exec.Language
	// MaxOutputBytes caps the stdout and stderr, each, a built sandbox
	// keeps. 0 means DefaultMaxOutputBytes. It does not apply to a Sandbox
	// passed in.
	MaxOutputBytes int

	// Workspace stores spilled results and backs the scratch tools. nil
	// means a new in-memory workspace.
	Workspace workspace.Workspace
	// Spill sets the spill threshold and preview size.
	Spill workspace.SpillOptions
	// NoSpill returns large results whole.
	NoSpill bool

	// FS adds options to the file tools, such as fs.WithMaxFileBytes.
	FS []fs.Option
	// Fetch adds options to fetch_url, such as fetch.AllowPrivateNetworks.
	Fetch []fetch.Option
}

// Toolset is what Harness returns.
type Toolset struct {
	// Tools are ready to register on an agent.
	Tools []types.Tool
	// Workspace holds spilled results and scratch artifacts. Attach it to
	// the agent (agent.WithWorkspace) so sub-agents get a read-only view.
	Workspace workspace.Workspace
	// Sandbox runs execute_code, or is nil when the exec group is off.
	Sandbox exec.Sandbox
}

// Names returns the tool names, in order.
func (s *Toolset) Names() []string {
	out := make([]string, len(s.Tools))
	for i, t := range s.Tools {
		out[i] = t.Definition().Name
	}
	return out
}

// coreTools are the tools worth sending on every turn when the rest are
// disclosed on demand.
var coreTools = []string{ReadFileName, ListDirName, GrepName, workspace.ReadToolName}

// Core returns the names to pin when the toolset is disclosed lazily with
// selector.DeferredTools: the read tools a model reaches for first. The
// other tools are found with tool_search.
func (s *Toolset) Core() []string {
	var out []string
	for _, name := range s.Names() {
		if slices.Contains(coreTools, name) {
			out = append(out, name)
		}
	}
	return out
}

// ErrNoRoot is returned when HarnessOptions has no Root.
var ErrNoRoot = errors.New("tools: a workspace root is required")

// Harness builds the toolset. ctx bounds setup work, such as probing the
// sandbox for languages.
func Harness(ctx context.Context, opts HarnessOptions) (*Toolset, error) {
	if opts.Root == "" {
		return nil, ErrNoRoot
	}
	groups := opts.Groups
	if len(groups) == 0 {
		groups = ReadOnly()
	}
	ws := opts.Workspace
	if ws == nil {
		ws = workspace.NewMemory()
	}
	set := &Toolset{Workspace: ws}

	var built []types.Tool
	if slices.Contains(groups, GroupRead) || slices.Contains(groups, GroupWrite) {
		fsOpts := slices.Clone(opts.FS)
		if slices.Contains(groups, GroupWrite) {
			fsOpts = append(fsOpts, fs.AllowWrites())
		}
		pack, err := fs.NewTools(opts.Root, fsOpts...)
		if err != nil {
			return nil, err
		}
		byName := map[string]types.Tool{}
		for _, t := range pack {
			byName[t.Definition().Name] = t
		}
		if slices.Contains(groups, GroupRead) {
			built = append(built,
				rename(byName["read"], ReadFileName, ""),
				rename(byName["list"], ListDirName, ""),
				rename(byName["glob"], GlobName, ""),
				rename(byName["grep"], GrepName, ""),
			)
		}
		if slices.Contains(groups, GroupWrite) {
			built = append(built,
				rename(byName["write"], WriteFileName, "Create or overwrite a file in the workspace with the given content. "+
					"Parent directories are created as needed. Prefer edit_file for changes to an existing file."),
				rename(byName["edit"], EditFileName, "Replace exact text in an existing workspace file. old_string must match the file exactly, "+
					"including whitespace, and must be unique unless replace_all is true. Read the file with read_file first."),
			)
		}
	}
	if slices.Contains(groups, GroupExec) {
		code, sb, err := codeTool(ctx, opts)
		if err != nil {
			return nil, err
		}
		set.Sandbox = sb
		built = append(built, code)
	}
	if slices.Contains(groups, GroupWeb) {
		built = append(built, rename(fetch.NewTool(opts.Fetch...), FetchURLName, ""))
	}
	if !opts.NoSpill {
		built = workspace.SpillAll(ws, opts.Spill, built...)
	}
	// The scratch tools are not spilled: scratch_read already pages.
	set.Tools = append(built, workspace.Tools(ws)...)
	return set, nil
}

// codeTool builds execute_code and the sandbox it runs on.
func codeTool(ctx context.Context, opts HarnessOptions) (types.Tool, exec.Sandbox, error) {
	policy := exec.DefaultPolicy()
	if opts.Policy != nil {
		policy = *opts.Policy
	}
	if opts.Network != "" {
		policy.Network = opts.Network
	}
	if policy.Network == "" {
		policy.Network = exec.NetworkDeny
	}
	if policy.Network != exec.NetworkDeny && policy.Network != exec.NetworkAllow {
		return nil, nil, fmt.Errorf("tools: network policy must be %q or %q, got %q", exec.NetworkDeny, exec.NetworkAllow, policy.Network)
	}
	if opts.Timeout > 0 {
		policy.Timeout = opts.Timeout
	}
	if opts.MaxTimeout > 0 {
		policy.MaxTimeout = opts.MaxTimeout
	}
	maxOut := opts.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = DefaultMaxOutputBytes
	}

	sb := opts.Sandbox
	if sb == nil {
		var err error
		switch opts.SandboxKind {
		case "", SandboxSubprocess:
			sbOpts := []exec.SubprocessOption{exec.WithMaxOutput(maxOut)}
			if policy.Network == exec.NetworkDeny {
				sbOpts = append(sbOpts, exec.NoNetwork())
			}
			sb, err = exec.NewSubprocess(sbOpts...)
		case SandboxDocker:
			dOpts := []exec.DockerOption{exec.DockerMaxOutput(maxOut)}
			if opts.DockerImage != "" {
				dOpts = append(dOpts, exec.DockerImage(opts.DockerImage))
			}
			if policy.Network == exec.NetworkAllow {
				dOpts = append(dOpts, exec.DockerAllowNetwork())
			}
			var d *exec.Docker
			if d, err = exec.NewDocker(opts.Root, dOpts...); err == nil {
				err = d.CheckMount(ctx)
			}
			sb = d
		default:
			err = fmt.Errorf("tools: unknown sandbox %q (want subprocess or docker)", opts.SandboxKind)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("tools: execute_code sandbox: %w", err)
		}
	}
	code, err := exec.NewCodeTool(ctx, sb, opts.Root, policy, exec.CodeOptions{Languages: opts.Languages})
	if err != nil {
		return nil, nil, err
	}
	return code, sb, nil
}

// rename gives a pack tool its harness name, and a new description when
// desc is not empty. Approval markers stay outermost and name the new tool.
func rename(t types.Tool, name, desc string) types.Tool {
	if mt, ok := t.(*types.MarkedTool); ok {
		markers := make([]types.Marker, len(mt.Markers))
		for i, m := range mt.Markers {
			if m.Meta != nil {
				m.Meta = maps.Clone(m.Meta)
				m.Meta["tool"] = name
			}
			if j := strings.LastIndex(m.Message, ": "); j >= 0 {
				m.Message = m.Message[:j+2] + name
			}
			markers[i] = m
		}
		return types.WithMarkers(rename(mt.Inner, name, desc), markers...)
	}
	def := t.Definition()
	def.Name = name
	if desc != "" {
		def.Description = desc
	}
	return &renamed{inner: t, def: def}
}

type renamed struct {
	inner types.Tool
	def   types.ToolDef
}

func (r *renamed) Definition() types.ToolDef { return r.def }

func (r *renamed) Execute(ctx context.Context, args map[string]any) (string, error) {
	return r.inner.Execute(ctx, args)
}

// Unwrap returns the pack tool.
func (r *renamed) Unwrap() types.Tool { return r.inner }
