package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/urmzd/saige/agent/definition"
	"github.com/urmzd/saige/agent/definition/bind"
	"github.com/urmzd/saige/agent/mcp"
	"github.com/urmzd/saige/agent/skills"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/tools"
	"github.com/urmzd/saige/tools/exec"
)

// envTrustProjectAgents trusts the project's .saige/agents directory.
const envTrustProjectAgents = "SAIGE_TRUST_PROJECT_AGENTS"

// agentFlags are the persistent flags that find agent definitions and the
// MCP servers they may name.
var agentFlags = struct {
	dirs      *[]string
	mcpConfig *string
}{dirs: new([]string), mcpConfig: new(string)}

func addAgentPersistentFlags(cmd *cobra.Command) {
	pf := cmd.PersistentFlags()
	pf.StringArrayVar(agentFlags.dirs, "agents-dir", nil, "Directory of agent definitions (repeatable; searched after ~/.config/saige/agents and .saige/agents)")
	pf.StringVar(agentFlags.mcpConfig, "mcp-config", "", "MCP configuration file (mcpServers) whose servers agent definitions may name")
}

// agentLayer is one directory of definitions, lowest precedence first.
type agentLayer struct {
	Kind    string `json:"kind"`
	Ref     string `json:"ref"`
	Trusted bool   `json:"trusted"`
}

// discoverAgentLayers lists the definition directories, lowest precedence
// first: the user directory, the project's .saige/agents, then each
// --agents-dir. A project directory can arrive with a cloned repository, so
// it is untrusted (see definition.UntrustedFields) unless
// SAIGE_TRUST_PROJECT_AGENTS=1 or the same directory is named with
// --agents-dir.
func discoverAgentLayers(dirs []string, getenv func(string) string) []agentLayer {
	var layers []agentLayer
	if dir := userAgentsDir(getenv); dir != "" && dirExists(dir) {
		layers = append(layers, agentLayer{Kind: layerUser, Ref: dir, Trusted: true})
	}
	explicit := map[string]bool{}
	for _, d := range dirs {
		explicit[cleanPath(d)] = true
	}
	if dir := projectAgentsDir(); dir != "" && !explicit[cleanPath(dir)] {
		layers = append(layers, agentLayer{Kind: layerProject, Ref: dir, Trusted: getenv(envTrustProjectAgents) == "1"})
	}
	for _, d := range dirs {
		layers = append(layers, agentLayer{Kind: layerFlag, Ref: d, Trusted: true})
	}
	return layers
}

func cleanPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// userAgentsDir is $XDG_CONFIG_HOME/saige/agents, defaulting to
// ~/.config/saige/agents.
func userAgentsDir(getenv func(string) string) string {
	dir := getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home := getenv("HOME")
		if home == "" {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "saige", "agents")
}

// projectAgentsDir walks up from the working directory to the first
// .saige/agents directory, stopping at a repository root.
func projectAgentsDir() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		if candidate := filepath.Join(dir, ".saige", "agents"); dirExists(candidate) {
			return candidate
		}
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func agentSource(layers []agentLayer, extra ...definition.Source) definition.Source {
	var sources []definition.Source
	for _, l := range layers {
		src := definition.DirSource(l.Ref)
		if !l.Trusted {
			src = definition.Untrusted(src)
		}
		sources = append(sources, src)
	}
	return definition.Layered(append(sources, extra...)...)
}

// skillCatalog loads the conventional skill locations: the project's
// .agents/skills and .claude/skills, then ~/.agents/skills.
func skillCatalog(ctx context.Context) (*skills.Catalog, error) {
	cwd, _ := os.Getwd()
	home, _ := os.UserHomeDir()
	return skills.NewCatalog(ctx, skills.StandardSources(cwd, home))
}

// loadAgents loads and checks every definition the flags reach, plus extra
// sources on top.
func loadAgents(ctx context.Context, cf *commonFlags, extra ...definition.Source) (*definition.Registry, []agentLayer, error) {
	cat, err := cf.catalog()
	if err != nil {
		return nil, nil, err
	}
	sk, err := skillCatalog(ctx)
	if err != nil {
		return nil, nil, err
	}
	layers := discoverAgentLayers(*agentFlags.dirs, os.Getenv)
	reg := definition.NewRegistry(agentSource(layers, extra...), definition.Checks{
		Model: bind.ModelCheck(cat),
		Skill: bind.SkillCheck(sk),
	})
	if _, err := reg.Load(ctx); err != nil {
		return nil, layers, invalidInput(err)
	}
	return reg, layers, nil
}

// agentRun is a definition bound for ask or chat.
type agentRun struct {
	bound   *bind.Bound
	cleanup func()
}

// agentHost holds what the CLI binds definitions with: the registry and
// the environment. serve binds one agent per session from it.
type agentHost struct {
	reg     *definition.Registry
	env     bind.Env
	cleanup func()
	// defaultPreset builds the CLI's default model, for a definition that
	// names none.
	defaultPreset func() (types.Preset, error)
}

// newAgentHost loads the definitions and prepares the CLI's environment:
// the catalog, the harness options for built-in tools, the rag and kg
// tools, the conventional skill locations and --mcp-config. --preset,
// --model and --provider, when given, override every root definition's
// model.
func newAgentHost(ctx context.Context, cmd *cobra.Command, cf *commonFlags, harness tools.HarnessOptions, verbose bool) (*agentHost, error) {
	for flag, what := range map[string]string{"tools": "tools", "system": "system prompt"} {
		if f := cmd.Flag(flag); f != nil && f.Changed {
			return nil, fmt.Errorf("--%s and --agent cannot be combined: the definition declares the agent's %s", flag, what)
		}
	}
	reg, _, err := loadAgents(ctx, cf)
	if err != nil {
		return nil, err
	}
	cat, err := cf.catalog()
	if err != nil {
		return nil, err
	}
	h := &agentHost{reg: reg, env: bind.Env{Catalog: cat, PresetOptions: cf.presetOptions(verbose), Harness: harness}}
	// Built at most once, and only when needed: serve binds sessions
	// concurrently.
	h.defaultPreset = sync.OnceValues(func() (types.Preset, error) { return resolveBundle(ctx, cf, verbose) })
	for _, flag := range []string{"preset", "model", "provider"} {
		if f := cmd.Flag(flag); f != nil && f.Changed {
			if h.env.Preset, err = h.defaultPreset(); err != nil {
				return nil, err
			}
			break
		}
	}
	registryTools, cleanup, err := buildTools(ctx, cf)
	if err != nil {
		return nil, err
	}
	h.cleanup = cleanup
	if len(registryTools) > 0 {
		h.env.Tools = types.NewToolRegistry(registryTools...)
	}
	if h.env.Skills, err = skillCatalog(ctx); err != nil {
		cleanup()
		return nil, err
	}
	if *agentFlags.mcpConfig != "" {
		specs, err := mcp.LoadConfig(*agentFlags.mcpConfig)
		if err != nil {
			cleanup()
			return nil, err
		}
		h.env.MCPServers = map[string]mcp.ServerSpec{}
		for _, s := range specs {
			h.env.MCPServers[s.Name] = s
		}
	}
	return h, nil
}

// bind resolves ref against the registry as it is now and binds it.
func (h *agentHost) bind(ctx context.Context, ref string) (*bind.Bound, error) {
	res, err := h.reg.Resolve(ref)
	if err != nil {
		return nil, invalidInput(fmt.Errorf("--agent %s: %w", ref, err))
	}
	env := h.env
	if env.Preset == nil && res.Model == nil {
		if env.Preset, err = h.defaultPreset(); err != nil {
			return nil, err
		}
	}
	b, err := bind.Bind(ctx, res, env)
	if err != nil {
		if errors.Is(err, bind.ErrUnsupported) && strings.Contains(err.Error(), "workspace root") {
			err = fmt.Errorf("%w; set --workspace", err)
		}
		return nil, fmt.Errorf("--agent %s: %w", ref, err)
	}
	return b, nil
}

// bindCLIAgent resolves and binds one definition for ask or chat.
func bindCLIAgent(ctx context.Context, cmd *cobra.Command, cf *commonFlags, ref string, harness tools.HarnessOptions, verbose bool) (*agentRun, error) {
	h, err := newAgentHost(ctx, cmd, cf, harness, verbose)
	if err != nil {
		return nil, err
	}
	b, err := h.bind(ctx, ref)
	if err != nil {
		h.cleanup()
		return nil, err
	}
	return &agentRun{bound: b, cleanup: func() { _ = b.Close(); h.cleanup() }}, nil
}

// cliHarness is the harness configuration of ask and chat.
func (h harnessFlags) options() tools.HarnessOptions {
	return tools.HarnessOptions{Root: h.workspace, SandboxKind: tools.SandboxKind(h.sandbox), Network: exec.NetworkPolicy(h.network)}
}

func newAgentCmd(ctx context.Context) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "List, inspect and validate agent definitions",
		Long: "An agent definition is a Markdown file with YAML frontmatter: *.agent.md, or\n" +
			"any .md file in a directory named agents. Directories are searched lowest\n" +
			"precedence first: ~/.config/saige/agents, the project's .saige/agents\n" +
			"(untrusted unless " + envTrustProjectAgents + "=1), then each --agents-dir.\n" +
			"Run one with saige ask --agent NAME[@RANGE] or saige chat --agent NAME.",
	}
	cmd.AddCommand(newAgentListCmd(ctx), newAgentShowCmd(ctx), newAgentValidateCmd(ctx), newAgentSchemaCmd())
	return cmd
}

// listedAgent is the JSON shape of one definition in agent list.
type listedAgent struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	Source      string `json:"source"`
	Path        string `json:"path,omitempty"`
	Trusted     bool   `json:"trusted"`
	Digest      string `json:"digest"`
}

func listed(d *definition.Definition) listedAgent {
	return listedAgent{Name: d.Name, Version: d.Version, Description: d.Description, Source: d.Source, Path: d.Path, Trusted: d.Trusted, Digest: d.Digest}
}

func newAgentListCmd(ctx context.Context) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List every agent definition, highest version first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cf := persistentFlagVars
			reg, layers, err := loadAgents(ctx, cf)
			if err != nil {
				return err
			}
			defs := reg.List()
			w := cmd.OutOrStdout()
			if cf.isJSON() {
				out := struct {
					Layers []agentLayer  `json:"layers"`
					Agents []listedAgent `json:"agents"`
				}{Layers: layers, Agents: []listedAgent{}}
				for _, d := range defs {
					out.Agents = append(out.Agents, listed(d))
				}
				return writeJSONTo(w, out)
			}
			if len(defs) == 0 {
				_, err := fmt.Fprintln(w, "no agent definitions found; add *.agent.md files to ~/.config/saige/agents, .saige/agents or --agents-dir")
				return err
			}
			tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "NAME\tVERSION\tDESCRIPTION\tSOURCE")
			for _, d := range defs {
				src := d.Location()
				if !d.Trusted {
					src += " (untrusted)"
				}
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", d.Name, d.Version, oneLine(d.Description, 60), src)
			}
			return tw.Flush()
		},
	}
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n-3] + "..."
	}
	return s
}

// shownAgent is the JSON shape of agent show.
type shownAgent struct {
	listedAgent
	// ResolvedDigest covers the definition and every sub-agent.
	ResolvedDigest string                 `json:"resolved_digest"`
	Definition     *definition.Definition `json:"definition"`
	Prompt         string                 `json:"prompt"`
	Subagents      []shownSubagent        `json:"subagents,omitempty"`
	Skills         []shownSkill           `json:"skills,omitempty"`
}

type shownSubagent struct {
	Ref     string `json:"ref"`
	Mode    string `json:"mode"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Digest  string `json:"digest"`
}

type shownSkill struct {
	Name string `json:"name"`
	Mode string `json:"mode"`
	Hash string `json:"hash,omitempty"`
}

func newAgentShowCmd(ctx context.Context) *cobra.Command {
	return &cobra.Command{
		Use:   "show NAME[@RANGE]",
		Short: "Print the definition a reference resolves to, with its digests",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cf := persistentFlagVars
			reg, _, err := loadAgents(ctx, cf)
			if err != nil {
				return err
			}
			res, err := reg.Resolve(args[0])
			if err != nil {
				return invalidInput(err)
			}
			out := shownAgent{listedAgent: listed(res.Definition), ResolvedDigest: res.Digest, Definition: res.Definition, Prompt: res.Prompt}
			for _, s := range res.Subagents {
				out.Subagents = append(out.Subagents, shownSubagent{Ref: s.Ref, Mode: s.EffectiveMode(), Name: s.Agent.Name,
					Version: s.Agent.Version, Digest: s.Agent.Digest})
			}
			if sk, err := skillCatalog(ctx); err == nil {
				for _, ref := range res.Skills {
					shown := shownSkill{Name: ref.Name, Mode: string(ref.EffectiveMode())}
					if s, err := sk.Load(ctx, res.Name, ref.Name); err == nil {
						shown.Hash = s.Hash
					}
					out.Skills = append(out.Skills, shown)
				}
			}
			w := cmd.OutOrStdout()
			if cf.isJSON() {
				return writeJSONTo(w, out)
			}
			fmt.Fprintf(w, "%s@%s\n", res.Name, res.Version)
			fmt.Fprintf(w, "source:   %s\n", res.Location())
			fmt.Fprintf(w, "digest:   %s\n", res.Definition.Digest)
			fmt.Fprintf(w, "resolved: %s\n", res.Digest)
			for _, s := range out.Subagents {
				fmt.Fprintf(w, "subagent: %s -> %s@%s (%s)\n", s.Ref, s.Name, s.Version, s.Mode)
			}
			for _, s := range out.Skills {
				fmt.Fprintf(w, "skill:    %s (%s) %s\n", s.Name, s.Mode, s.Hash)
			}
			fmt.Fprintln(w)
			_, err = w.Write(res.Raw())
			return err
		},
	}
}

func newAgentValidateCmd(ctx context.Context) *cobra.Command {
	return &cobra.Command{
		Use:   "validate [PATH...]",
		Short: "Check definitions offline: syntax, fields, references, cycles and trust",
		Long: "Validate every definition the agent directories hold, and each PATH: a\n" +
			"definition file or a directory of them. Files named on the command line\n" +
			"are checked together with the directories, so their references resolve.\n" +
			"Exits 2 when anything is invalid.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cf := persistentFlagVars
			var extra []definition.Source
			for _, p := range args {
				src, err := pathSource(p)
				if err != nil {
					return invalidInput(err)
				}
				extra = append(extra, src)
			}
			reg, _, err := loadAgents(ctx, cf, extra...)
			if err != nil {
				return err
			}
			defs := reg.List()
			for _, d := range defs {
				if _, err := reg.Resolve(d.ID()); err != nil {
					return invalidInput(err)
				}
			}
			w := cmd.OutOrStdout()
			if cf.isJSON() {
				return writeJSONTo(w, map[string]any{"valid": true, "definitions": len(defs)})
			}
			_, err = fmt.Fprintf(w, "%d agent definitions valid\n", len(defs))
			return err
		},
	}
}

// pathSource reads one definition file, or a directory of them.
func pathSource(p string) (definition.Source, error) {
	info, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return definition.DirSource(p), nil
	}
	return definition.ReaderSource(p, func(context.Context) (io.ReadCloser, error) {
		return os.Open(p) //nolint:gosec // the path is the user's own argument
	}), nil
}

func newAgentSchemaCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "schema",
		Short: "Print the JSON Schema of the definition frontmatter",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := cmd.OutOrStdout().Write(definition.Schema())
			return err
		},
	}
}

func writeJSONTo(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
