package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/urmzd/saige/agent/definition"
	"github.com/urmzd/saige/agent/provider"
	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/skills"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/tools"
)

// Harnesses saige exports to and launches.
const (
	harnessClaude   = "claude"
	harnessCodex    = "codex"
	harnessGemini   = "gemini"
	harnessOpencode = "opencode"
	harnessCursor   = "cursor"
	// harnessSkills writes only the skills, to every conventional
	// directory.
	harnessSkills = "skills"
)

var exportHarnesses = []string{harnessClaude, harnessCodex, harnessGemini, harnessOpencode, harnessCursor, harnessSkills}

// exportedBy marks skills saige export wrote, in their metadata, so a later
// export may replace them and never a skill someone else wrote.
const (
	exportedByKey   = "generated-by"
	exportedByValue = "saige export"
)

// exportFlags are the flags export and launch share.
type exportFlags struct {
	agent      string
	dir        string
	user       bool
	dryRun     bool
	diff       bool
	force      bool
	mcpCommand string
}

func addExportFlags(cmd *cobra.Command, f *exportFlags) {
	fl := cmd.Flags()
	fl.StringVar(&f.agent, "agent", "", "Agent definition to export: NAME or NAME@RANGE (required)")
	fl.StringVar(&f.dir, "dir", ".", "Project directory to write the harness's project configuration in")
	fl.BoolVar(&f.user, "user", false, "Write the harness's user-level configuration instead (always prints the diff)")
	fl.BoolVar(&f.dryRun, "dry-run", false, "Print what would change, with diffs, and write nothing")
	fl.BoolVar(&f.diff, "diff", false, "Print the diff of every changed file")
	fl.BoolVar(&f.force, "force", false, "Replace a skill of the same name that saige export did not write")
	fl.StringVar(&f.mcpCommand, "mcp-command", "saige-mcp", "Command the harness runs for the saige MCP server")
}

func newExportCmd(ctx context.Context) *cobra.Command {
	var f exportFlags
	cmd := &cobra.Command{
		Use:       "export HARNESS",
		Short:     "Write a harness's configuration so it can use a saige agent",
		ValidArgs: exportHarnesses,
		Long: `Export an agent definition to a coding harness: claude (Claude Code), codex,
gemini (Gemini CLI), opencode, cursor (Cursor agent), or skills (the skills
only, to .agents/skills, .claude/skills and .kiro/skills).

Export writes, in the project directory:

  - an MCP server entry, saige-NAME, that runs saige-mcp --agents-dir DIR
    --agent NAME, so the harness can hand tasks to the saige agent;
  - the definition's skills, with only the agentskills.io frontmatter
    fields (name, description, license, compatibility, metadata,
    allowed-tools);
  - the definition in the harness's own agent format, with a note for
    every field the format cannot express.

Existing configuration is merged: other servers and keys are kept, and
running export again changes nothing. --user writes the harness's
user-level files instead and always prints the diff. --dry-run prints what
would change and writes nothing.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			plan, base, err := planExport(ctx, args[0], f)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			plan.report(w, base, f.dryRun, f.dryRun || f.diff || f.user)
			if f.dryRun {
				return nil
			}
			return plan.apply()
		},
	}
	addExportFlags(cmd, &f)
	return cmd
}

// exportSource is a resolved definition with what an export needs.
type exportSource struct {
	res        *definition.Resolved
	agentsDir  string
	skills     []skills.Skill
	cat        *catalog.Catalog
	mcpCommand string
	// root is the --root saige-mcp confines harness tools to.
	root string
}

// serverName is the MCP server's name in every harness.
func (s *exportSource) serverName() string { return "saige-" + s.res.Name }

// mcpArgs are saige-mcp's arguments for the agent.
func (s *exportSource) mcpArgs() []string {
	args := []string{"--agents-dir", s.agentsDir, "--agent", s.res.Name, "--root", s.root}
	if s.res.Tools == nil || len(s.res.Tools.Registry) == 0 {
		// Only the agent tool: no pack tools of saige-mcp's own.
		args = append(args, "--tools", "none")
	}
	return args
}

// loadExportSource resolves the definition and its skills. A definition
// that is not trusted, or that does not come from a directory saige-mcp
// can read, is refused.
func loadExportSource(ctx context.Context, f exportFlags, root string) (*exportSource, error) {
	if f.agent == "" {
		return nil, invalidInput(errors.New("--agent is required"))
	}
	cf := persistentFlagVars
	reg, _, err := loadAgents(ctx, cf)
	if err != nil {
		return nil, err
	}
	res, err := reg.Resolve(f.agent)
	if err != nil {
		return nil, invalidInput(fmt.Errorf("--agent %s: %w", f.agent, err))
	}
	if !res.Trusted {
		return nil, invalidInput(fmt.Errorf("agent %s comes from an untrusted directory (%s); trust it with %s=1 or --agents-dir before exporting it",
			res.Name, res.Location(), envTrustProjectAgents))
	}
	dir := res.Source
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil, invalidInput(fmt.Errorf("agent %s was not loaded from a directory (%s); saige-mcp reads definitions from --agents-dir", res.Name, res.Location()))
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return nil, err
	}
	cat, err := cf.catalog()
	if err != nil {
		return nil, err
	}
	src := &exportSource{res: res, agentsDir: dir, cat: cat, mcpCommand: f.mcpCommand, root: root}
	if len(res.Skills) > 0 {
		sk, err := skillCatalog(ctx)
		if err != nil {
			return nil, err
		}
		for _, ref := range res.Skills {
			s, err := sk.Load(ctx, res.Name, ref.Name)
			if err != nil {
				return nil, invalidInput(fmt.Errorf("agent %s: skill %s: %w", res.Name, ref.Name, err))
			}
			src.skills = append(src.skills, s)
		}
	}
	return src, nil
}

// exportBase returns the directory export writes under: the project, or
// the home directory with --user.
func exportBase(f exportFlags) (base, root string, err error) {
	project, err := filepath.Abs(f.dir)
	if err != nil {
		return "", "", err
	}
	if !f.user {
		return project, project, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	// A user-level server serves whichever project the harness runs in.
	return home, ".", nil
}

// planExport builds the plan for one harness.
func planExport(ctx context.Context, harness string, f exportFlags) (*exportPlan, string, error) {
	if !slices.Contains(exportHarnesses, harness) {
		return nil, "", invalidInput(fmt.Errorf("unknown harness %q: use one of %s", harness, strings.Join(exportHarnesses, ", ")))
	}
	base, root, err := exportBase(f)
	if err != nil {
		return nil, "", err
	}
	src, err := loadExportSource(ctx, f, root)
	if err != nil {
		return nil, "", err
	}
	plan := &exportPlan{}
	if err := src.plan(plan, harness, base, f); err != nil {
		return nil, "", err
	}
	return plan, base, nil
}

// harnessPaths are where a harness reads each kind of file, relative to
// the export base.
type harnessPaths struct {
	mcp, skills, agents string
}

func pathsFor(harness string, user bool) harnessPaths {
	if user {
		switch harness {
		case harnessClaude:
			return harnessPaths{".claude.json", ".claude/skills", ".claude/agents"}
		case harnessCodex:
			return harnessPaths{".codex/config.toml", ".agents/skills", ".codex/agents"}
		case harnessGemini:
			return harnessPaths{".gemini/settings.json", ".agents/skills", ".gemini/agents"}
		case harnessOpencode:
			return harnessPaths{".config/opencode/opencode.json", ".agents/skills", ".config/opencode/agents"}
		case harnessCursor:
			return harnessPaths{".cursor/mcp.json", ".agents/skills", ".cursor/agents"}
		}
		return harnessPaths{}
	}
	switch harness {
	case harnessClaude:
		return harnessPaths{".mcp.json", ".claude/skills", ".claude/agents"}
	case harnessCodex:
		return harnessPaths{".codex/config.toml", ".agents/skills", ".codex/agents"}
	case harnessGemini:
		return harnessPaths{".gemini/settings.json", ".agents/skills", ".gemini/agents"}
	case harnessOpencode:
		return harnessPaths{"opencode.json", ".agents/skills", ".opencode/agents"}
	case harnessCursor:
		return harnessPaths{".cursor/mcp.json", ".agents/skills", ".cursor/agents"}
	}
	return harnessPaths{}
}

func (s *exportSource) plan(plan *exportPlan, harness, base string, f exportFlags) error {
	if harness == harnessSkills {
		for _, dir := range []string{".agents/skills", ".claude/skills", ".kiro/skills"} {
			if err := s.planSkills(plan, filepath.Join(base, dir), f.force); err != nil {
				return err
			}
		}
		if len(s.skills) == 0 {
			plan.note("agent %s names no skills", s.res.Name)
		}
		return nil
	}
	p := pathsFor(harness, f.user)
	if err := s.planMCP(plan, harness, filepath.Join(base, p.mcp)); err != nil {
		return err
	}
	if err := s.planSkills(plan, filepath.Join(base, p.skills), f.force); err != nil {
		return err
	}
	return s.planAgent(plan, harness, filepath.Join(base, p.agents))
}

// ── MCP entries ─────────────────────────────────────────────────────

// passEnv lists the provider credentials a harness that does not pass its
// environment to MCP servers must forward.
func passEnv() []string {
	set := map[string]bool{provider.EnvUseVertex: true, provider.EnvCloudProject: true, provider.EnvCloudLocation: true,
		"GOOGLE_APPLICATION_CREDENTIALS": true, "OLLAMA_HOST": true, "SAIGE_CATALOG": true}
	for _, envs := range provider.APIKeyEnv {
		for _, e := range envs {
			set[e] = true
		}
	}
	out := make([]string, 0, len(set))
	for e := range set {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}

// mcpEntry is the server entry in a harness's JSON format.
func (s *exportSource) mcpEntry(harness string) (path []string, value any, seed []jsonMember) {
	name, args := s.serverName(), s.mcpArgs()
	switch harness {
	case harnessOpencode:
		return []string{"mcp", name}, opencodeServer{Type: "local", Command: append([]string{s.mcpCommand}, args...), Enabled: true},
			[]jsonMember{{key: "$schema", value: []byte(`"https://opencode.ai/config.json"`)}}
	case harnessGemini:
		return []string{"mcpServers", name}, stdioServer{Command: s.mcpCommand, Args: args}, nil
	}
	return []string{"mcpServers", name}, stdioServer{Type: "stdio", Command: s.mcpCommand, Args: args}, nil
}

// stdioServer is the mcpServers entry of Claude Code, Cursor and Gemini CLI.
type stdioServer struct {
	Type    string   `json:"type,omitempty"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// opencodeServer is opencode's mcp entry.
type opencodeServer struct {
	Type    string   `json:"type"`
	Command []string `json:"command"`
	Enabled bool     `json:"enabled"`
}

// codexTable is the server's table in Codex's config.toml.
func (s *exportSource) codexTable() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[mcp_servers.%s]\n", s.serverName())
	fmt.Fprintf(&b, "command = %s\n", tomlString(s.mcpCommand))
	fmt.Fprintf(&b, "args = %s\n", tomlStrings(s.mcpArgs()))
	// Codex starts MCP servers with a minimal environment; forward the
	// provider credentials the agent's model needs.
	fmt.Fprintf(&b, "env_vars = %s\n", tomlStrings(passEnv()))
	return b.String()
}

func (s *exportSource) planMCP(plan *exportPlan, harness, path string) error {
	raw, err := plan.current(path)
	if err != nil {
		return err
	}
	var out []byte
	if harness == harnessCodex {
		out, err = mergeTOMLTable(raw, "mcp_servers."+s.serverName(), "mcp_servers", s.codexTable())
		plan.note("Codex reads a project's .codex/config.toml only once the project is trusted; approvals reach you through elicitation when approval_policy allows mcp_elicitations, and through saige approvals otherwise")
	} else {
		keys, value, seed := s.mcpEntry(harness)
		out, err = mergeJSON(raw, keys, value, seed)
	}
	if err != nil {
		return invalidInput(fmt.Errorf("%s: %w", path, err))
	}
	if harness == harnessOpencode {
		plan.note("opencode has no MCP elicitation: an approval the agent needs is held, and you decide it with saige approvals")
	}
	return plan.set(path, out)
}

// ── skills ──────────────────────────────────────────────────────────

// skillFrontmatter is the agentskills.io frontmatter, in its field order.
type skillFrontmatter struct {
	Name          string            `yaml:"name"`
	Description   string            `yaml:"description"`
	License       string            `yaml:"license,omitempty"`
	Compatibility string            `yaml:"compatibility,omitempty"`
	Metadata      map[string]string `yaml:"metadata,omitempty"`
	AllowedTools  string            `yaml:"allowed-tools,omitempty"`
}

// skillFile renders a skill's SKILL.md with only the spec's fields.
func skillFile(s skills.Skill) ([]byte, error) {
	meta := map[string]string{}
	for k, v := range s.Metadata {
		meta[k] = v
	}
	meta[exportedByKey] = exportedByValue
	fm := skillFrontmatter{Name: s.Name, Description: s.Description, License: s.License, Compatibility: s.Compatibility,
		Metadata: meta, AllowedTools: strings.Join(s.AllowedTools, " ")}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(fm); err != nil {
		return nil, err
	}
	head := buf.Bytes()
	body := strings.TrimLeft(s.Body, "\n")
	return []byte("---\n" + string(head) + "---\n\n" + body), nil
}

func (s *exportSource) planSkills(plan *exportPlan, dir string, force bool) error {
	for _, sk := range s.skills {
		target := filepath.Join(dir, sk.Name)
		// The skill already lives here: it is its own source.
		if sk.Source != "" && cleanPath(sk.Source) == cleanPath(dir) {
			continue
		}
		existing, err := plan.current(filepath.Join(target, "SKILL.md"))
		if err != nil {
			return err
		}
		if existing != nil && !force && !exportedSkill(existing) {
			return invalidInput(fmt.Errorf("%s holds a skill saige export did not write; remove it or pass --force", target))
		}
		md, err := skillFile(sk)
		if err != nil {
			return err
		}
		if err := plan.set(filepath.Join(target, "SKILL.md"), md); err != nil {
			return err
		}
		for _, r := range sk.Resources {
			raw, _, err := sk.ReadResource(r.Path, 0)
			if err != nil {
				return err
			}
			path := filepath.Join(target, filepath.FromSlash(r.Path))
			if err := plan.set(path, raw); err != nil {
				return err
			}
			if bytes.HasPrefix(raw, []byte("#!")) {
				plan.executable(path)
			}
		}
	}
	return nil
}

func exportedSkill(raw []byte) bool {
	fm, _, err := skills.ParseSkillFile(raw)
	if err != nil {
		return false
	}
	meta, _ := fm["metadata"].(map[string]string)
	return meta[exportedByKey] == exportedByValue
}

// ── agent definitions ───────────────────────────────────────────────

// nativeModel returns the definition's provider and model ID, resolving a
// preset to its first entry.
func (s *exportSource) nativeModel() (types.ProviderName, string) {
	if s.res.Model == nil {
		return "", ""
	}
	if !s.res.Model.IsPreset() {
		p, m, _ := strings.Cut(s.res.Model.Use, "/")
		return types.ProviderName(p), m
	}
	rp, err := s.cat.Resolve(types.PresetName(s.res.Model.Use))
	if err != nil || len(rp.Chain) == 0 {
		return "", ""
	}
	return rp.Chain[0].Provider, string(rp.Chain[0].Model)
}

func (s *exportSource) hasGroup(g tools.Group) bool {
	return s.res.Tools != nil && slices.Contains(s.res.Tools.Harness, string(g))
}

// unexpressed notes the definition fields no harness agent format holds;
// the agent still has them when the harness calls it over MCP.
func (s *exportSource) unexpressed(plan *exportPlan, harness string, extra ...string) {
	d := s.res
	var fields []string
	add := func(name string, set bool) {
		if set {
			fields = append(fields, name)
		}
	}
	add("dials", d.Dials != nil)
	add("memory", d.Memory != nil)
	add("guardrails", d.Guardrails != nil)
	add("compaction", d.Compaction != nil)
	add("limits.budget", d.Limits != nil && d.Limits.Budget != nil)
	add("subagents", len(d.Subagents) > 0)
	add("tools.registry", d.Tools != nil && len(d.Tools.Registry) > 0)
	add("approval", d.Approval != nil)
	fields = append(fields, extra...)
	if len(fields) == 0 {
		return
	}
	plan.note("%s's agent format cannot express %s of agent %s; the %s MCP tool runs the agent with all of them",
		harness, strings.Join(fields, ", "), d.Name, s.serverName())
}

func (s *exportSource) description() string {
	if s.res.Description != "" {
		return s.res.Description
	}
	return "The " + s.res.Name + " agent."
}

func (s *exportSource) planAgent(plan *exportPlan, harness, dir string) error {
	var (
		name = s.res.Name + ".md"
		out  []byte
		err  error
	)
	switch harness {
	case harnessClaude:
		out, err = s.claudeAgent(plan)
	case harnessCursor:
		out, err = s.cursorAgent(plan)
	case harnessCodex:
		name = s.res.Name + ".toml"
		out = s.codexAgent(plan)
	case harnessOpencode:
		out, err = s.opencodeAgent(plan)
	case harnessGemini:
		out, err = s.geminiAgent(plan)
	}
	if err != nil {
		return err
	}
	return plan.set(filepath.Join(dir, name), out)
}

// markdownAgent renders frontmatter, given as ordered key and value
// pairs, and the prompt as the body.
func markdownAgent(prompt string, kv ...any) ([]byte, error) {
	node := &yaml.Node{Kind: yaml.MappingNode}
	for i := 0; i+1 < len(kv); i += 2 {
		var v yaml.Node
		if err := v.Encode(kv[i+1]); err != nil {
			return nil, err
		}
		node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: kv[i].(string)}, &v)
	}
	var head bytes.Buffer
	enc := yaml.NewEncoder(&head)
	enc.SetIndent(2)
	if err := enc.Encode(node); err != nil {
		return nil, err
	}
	return []byte("---\n" + head.String() + "---\n\n" + strings.TrimSpace(prompt) + "\n"), nil
}

// claudeTools maps harness groups and MCP references to Claude Code tool
// names.
func (s *exportSource) claudeTools() []string {
	var out []string
	groups := map[tools.Group][]string{
		tools.GroupRead:  {"Read", "Grep", "Glob"},
		tools.GroupWrite: {"Write", "Edit"},
		tools.GroupExec:  {"Bash"},
		tools.GroupWeb:   {"WebFetch"},
	}
	for _, g := range tools.AllGroups() {
		if s.hasGroup(g) {
			out = append(out, groups[g]...)
		}
	}
	if s.res.Tools != nil {
		for _, ref := range s.res.Tools.MCP {
			if len(ref.Allow) == 0 {
				out = append(out, "mcp__"+ref.Server)
				continue
			}
			for _, t := range ref.Allow {
				out = append(out, "mcp__"+ref.Server+"__"+t)
			}
		}
	}
	return out
}

func (s *exportSource) claudeAgent(plan *exportPlan) ([]byte, error) {
	kv := []any{"name", s.res.Name, "description", s.description()}
	var extra []string
	if t := s.claudeTools(); len(t) > 0 {
		kv = append(kv, "tools", strings.Join(t, ", "))
	} else {
		extra = append(extra, "an empty tool set (a Claude Code agent without tools inherits every tool)")
	}
	if p, m := s.nativeModel(); p == provider.Anthropic && m != "" {
		kv = append(kv, "model", m)
	} else if s.res.Model != nil {
		extra = append(extra, "model "+s.res.Model.Use)
	}
	if len(s.res.Skills) > 0 {
		var names []string
		for _, sk := range s.res.Skills {
			names = append(names, sk.Name)
		}
		kv = append(kv, "skills", names)
	}
	if s.res.Limits != nil && s.res.Limits.MaxIterations > 0 {
		kv = append(kv, "maxTurns", s.res.Limits.MaxIterations)
	}
	if s.res.Tools != nil && len(s.res.Tools.MCP) > 0 {
		plan.note("agent %s names MCP servers %s; Claude Code must configure servers of the same names for its agent to reach them", s.res.Name, mcpNames(s.res.Tools.MCP))
	}
	s.unexpressed(plan, "Claude Code", extra...)
	return markdownAgent(s.res.Prompt, kv...)
}

func mcpNames(refs []definition.MCPRef) string {
	var names []string
	for _, r := range refs {
		names = append(names, r.Server)
	}
	return strings.Join(names, ", ")
}

func (s *exportSource) cursorAgent(plan *exportPlan) ([]byte, error) {
	kv := []any{"name", s.res.Name, "description", s.description()}
	readonly := !s.hasGroup(tools.GroupWrite) && !s.hasGroup(tools.GroupExec)
	kv = append(kv, "readonly", readonly)
	var extra []string
	if s.res.Model != nil {
		extra = append(extra, "model "+s.res.Model.Use)
	}
	if s.res.Tools != nil && (len(s.res.Tools.Harness) > 0 || len(s.res.Tools.MCP) > 0) {
		extra = append(extra, "per-tool selection (only readonly)")
	}
	if len(s.res.Skills) > 0 {
		extra = append(extra, "skills (Cursor finds them in .agents/skills)")
	}
	s.unexpressed(plan, "Cursor", extra...)
	return markdownAgent(s.res.Prompt, kv...)
}

func (s *exportSource) codexAgent(plan *exportPlan) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "name = %s\n", tomlString(s.res.Name))
	fmt.Fprintf(&b, "description = %s\n", tomlString(s.description()))
	var extra []string
	if p, m := s.nativeModel(); p == provider.OpenAI && m != "" {
		fmt.Fprintf(&b, "model = %s\n", tomlString(m))
	} else if s.res.Model != nil {
		extra = append(extra, "model "+s.res.Model.Use)
	}
	sandbox := "read-only"
	if s.hasGroup(tools.GroupWrite) || s.hasGroup(tools.GroupExec) {
		sandbox = "workspace-write"
	}
	fmt.Fprintf(&b, "sandbox_mode = %s\n", tomlString(sandbox))
	if s.res.Tools != nil && len(s.res.Tools.MCP) > 0 {
		extra = append(extra, "tools.mcp")
	}
	if s.res.Limits != nil && s.res.Limits.MaxIterations > 0 {
		extra = append(extra, "limits.max_iterations")
	}
	fmt.Fprintf(&b, "developer_instructions = %s\n", tomlText(strings.TrimSpace(s.res.Prompt)+"\n"))
	s.unexpressed(plan, "Codex", extra...)
	return []byte(b.String())
}

// opencodeProviders maps saige providers to opencode's provider IDs.
var opencodeProviders = map[types.ProviderName]string{provider.Anthropic: "anthropic", provider.OpenAI: "openai", provider.Google: "google", provider.Ollama: "ollama"}

func (s *exportSource) opencodeConfig(plan *exportPlan) map[string]any {
	cfg := map[string]any{"description": s.description(), "mode": "all"}
	var extra []string
	if p, m := s.nativeModel(); opencodeProviders[p] != "" && m != "" {
		cfg["model"] = opencodeProviders[p] + "/" + m
	} else if s.res.Model != nil {
		extra = append(extra, "model "+s.res.Model.Use)
	}
	allow := func(set bool, yes string) string {
		if set {
			return yes
		}
		return "deny"
	}
	cfg["permission"] = map[string]any{
		"edit":     allow(s.hasGroup(tools.GroupWrite), "ask"),
		"bash":     allow(s.hasGroup(tools.GroupExec), "ask"),
		"webfetch": allow(s.hasGroup(tools.GroupWeb), "allow"),
	}
	if s.res.Limits != nil && s.res.Limits.MaxIterations > 0 {
		cfg["steps"] = s.res.Limits.MaxIterations
	}
	if s.res.Tools != nil && len(s.res.Tools.MCP) > 0 {
		extra = append(extra, "tools.mcp")
	}
	if plan != nil {
		s.unexpressed(plan, "opencode", extra...)
	}
	return cfg
}

func (s *exportSource) opencodeAgent(plan *exportPlan) ([]byte, error) {
	cfg := s.opencodeConfig(plan)
	kv := []any{"description", cfg["description"], "mode", cfg["mode"]}
	if m, ok := cfg["model"]; ok {
		kv = append(kv, "model", m)
	}
	if st, ok := cfg["steps"]; ok {
		kv = append(kv, "steps", st)
	}
	kv = append(kv, "permission", cfg["permission"])
	return markdownAgent(s.res.Prompt, kv...)
}

func (s *exportSource) geminiAgent(plan *exportPlan) ([]byte, error) {
	kv := []any{"name", s.res.Name, "description", s.description(), "kind", "local"}
	groups := map[tools.Group][]string{
		tools.GroupRead:  {"read_file", "list_directory", "glob", "search_file_content"},
		tools.GroupWrite: {"write_file", "replace"},
		tools.GroupExec:  {"run_shell_command"},
		tools.GroupWeb:   {"web_fetch"},
	}
	var names []string
	for _, g := range tools.AllGroups() {
		if s.hasGroup(g) {
			names = append(names, groups[g]...)
		}
	}
	if len(names) > 0 {
		kv = append(kv, "tools", names)
	}
	var extra []string
	if p, m := s.nativeModel(); p == provider.Google && m != "" {
		kv = append(kv, "model", m)
	} else if s.res.Model != nil {
		extra = append(extra, "model "+s.res.Model.Use)
	}
	if s.res.Limits != nil && s.res.Limits.MaxIterations > 0 {
		kv = append(kv, "max_turns", s.res.Limits.MaxIterations)
	}
	if s.res.Tools != nil && len(s.res.Tools.MCP) > 0 {
		extra = append(extra, "tools.mcp")
	}
	s.unexpressed(plan, "Gemini CLI", extra...)
	return markdownAgent(s.res.Prompt, kv...)
}
