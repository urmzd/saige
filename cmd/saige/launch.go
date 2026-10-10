package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"
)

// launchHarnesses are the harnesses saige launch starts.
var launchHarnesses = []string{harnessClaude, harnessCodex, harnessGemini, harnessOpencode, harnessCursor}

// harnessBinaries are the commands each harness installs, first found wins.
var harnessBinaries = map[string][]string{
	harnessClaude:   {"claude"},
	harnessCodex:    {"codex"},
	harnessGemini:   {"gemini"},
	harnessOpencode: {"opencode"},
	harnessCursor:   {"cursor-agent", "agent"},
}

// launch is how to start a harness with a saige agent.
type launch struct {
	bin  string
	args []string
	env  []string
	// plan holds the project files a harness without flags for them
	// needs; tempSkills the skills put in a temporary plugin directory.
	plan       *exportPlan
	tempSkills bool
}

func newLaunchCmd(ctx context.Context) *cobra.Command {
	var (
		f   exportFlags
		bin string
	)
	cmd := &cobra.Command{
		Use:   "launch HARNESS [-- HARNESS-ARGS...]",
		Short: "Start a harness with a saige agent available, without changing your configuration where it can",
		Long: `Start claude, codex, gemini, opencode or cursor (the Cursor agent CLI) with
the saige agent's MCP server, skills and definition, then pass the arguments
after -- to the harness.

Where the harness takes configuration on its command line or environment,
nothing is written: claude gets --mcp-config, --agents and a temporary
--plugin-dir for skills; codex gets -c overrides; opencode gets
OPENCODE_CONFIG_CONTENT. What a harness can only read from files (Gemini
CLI and Cursor MCP servers, and skills for codex, gemini, opencode and
cursor) is written to the project, as saige export would, and printed
first. User-level configuration is never written.

  saige launch claude --agent reviewer -- -p "review the last commit"
  saige launch opencode --agent reviewer -- run "review the last commit"`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			harness, rest := args[0], args[1:]
			if !slices.Contains(launchHarnesses, harness) {
				return invalidInput(fmt.Errorf("unknown harness %q: use one of %s", harness, strings.Join(launchHarnesses, ", ")))
			}
			project, err := filepath.Abs(f.dir)
			if err != nil {
				return err
			}
			src, err := loadExportSource(ctx, f, project)
			if err != nil {
				return err
			}
			l, err := src.launch(harness, project, f.force)
			if err != nil {
				return err
			}
			if bin != "" {
				l.bin = bin
			}
			l.args = append(l.args, rest...)
			errW := cmd.ErrOrStderr()
			l.plan.report(errW, project, f.dryRun, true)
			if f.dryRun {
				fmt.Fprintf(errW, "would run %s\n", shellJoin(append([]string{l.bin}, l.args...)))
				for _, e := range l.env {
					k, v, _ := strings.Cut(e, "=")
					fmt.Fprintf(errW, "  with %s=%s\n", k, v)
				}
				if l.tempSkills {
					fmt.Fprintf(errW, "  with a temporary --plugin-dir holding %d skills\n", len(src.skills))
				}
				return nil
			}
			if err := l.plan.apply(); err != nil {
				return err
			}
			if l.tempSkills {
				dir, err := src.claudePlugin()
				if err != nil {
					return err
				}
				defer func() { _ = os.RemoveAll(dir) }()
				l.args = append([]string{"--plugin-dir", dir}, l.args...)
			}
			return runHarness(cmd, l)
		},
	}
	cmd.Flags().StringVar(&f.agent, "agent", "", "Agent definition to launch with: NAME or NAME@RANGE (required)")
	cmd.Flags().StringVar(&f.dir, "dir", ".", "Project directory the harness runs in")
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "Print the command and the files it would write, and run nothing")
	cmd.Flags().BoolVar(&f.force, "force", false, "Replace a skill of the same name that saige export did not write")
	cmd.Flags().StringVar(&f.mcpCommand, "mcp-command", "saige-mcp", "Command the harness runs for the saige MCP server")
	cmd.Flags().StringVar(&bin, "bin", "", "Harness executable (default: found on PATH)")
	return cmd
}

// launch returns how to start harness in project.
func (s *exportSource) launch(harness, project string, force bool) (*launch, error) {
	l := &launch{plan: &exportPlan{}}
	for _, b := range harnessBinaries[harness] {
		if p, err := exec.LookPath(b); err == nil {
			l.bin = p
			break
		}
	}
	if l.bin == "" {
		l.bin = harnessBinaries[harness][0]
	}
	paths := pathsFor(harness, false)
	name := s.serverName()
	switch harness {
	case harnessClaude:
		_, entry, _ := s.mcpEntry(harness)
		servers, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{name: entry}})
		agent := map[string]any{"description": s.description(), "prompt": s.res.Prompt}
		if t := s.claudeTools(); len(t) > 0 {
			agent["tools"] = t
		}
		if p, m := s.nativeModel(); p == "anthropic" && m != "" {
			agent["model"] = m
		}
		agents, _ := json.Marshal(map[string]any{s.res.Name: agent})
		l.args = []string{"--mcp-config", string(servers), "--agents", string(agents)}
		l.tempSkills = len(s.skills) > 0
		return l, nil
	case harnessCodex:
		key := "mcp_servers." + name
		l.args = []string{
			"-c", key + ".command=" + tomlString(s.mcpCommand),
			"-c", key + ".args=" + tomlStrings(s.mcpArgs()),
			"-c", key + ".env_vars=" + tomlStrings(passEnv()),
		}
	case harnessOpencode:
		cfg := map[string]any{}
		if raw := os.Getenv("OPENCODE_CONFIG_CONTENT"); raw != "" {
			if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
				return nil, invalidInput(fmt.Errorf("OPENCODE_CONFIG_CONTENT: %w", err))
			}
		}
		_, entry, _ := s.mcpEntry(harness)
		mcpCfg, _ := cfg["mcp"].(map[string]any)
		if mcpCfg == nil {
			mcpCfg = map[string]any{}
		}
		mcpCfg[name] = entry
		cfg["mcp"] = mcpCfg
		agentCfg, _ := cfg["agent"].(map[string]any)
		if agentCfg == nil {
			agentCfg = map[string]any{}
		}
		a := s.opencodeConfig(nil)
		a["prompt"] = s.res.Prompt
		agentCfg[s.res.Name] = a
		cfg["agent"] = agentCfg
		if _, ok := cfg["$schema"]; !ok {
			cfg["$schema"] = "https://opencode.ai/config.json"
		}
		raw, _ := json.Marshal(cfg)
		l.env = []string{"OPENCODE_CONFIG_CONTENT=" + string(raw)}
	case harnessGemini, harnessCursor:
		if err := s.planMCP(l.plan, harness, filepath.Join(project, paths.mcp)); err != nil {
			return nil, err
		}
	}
	if err := s.planSkills(l.plan, filepath.Join(project, paths.skills), force); err != nil {
		return nil, err
	}
	return l, nil
}

// claudePlugin writes the skills to a temporary Claude Code plugin.
func (s *exportSource) claudePlugin() (string, error) {
	dir, err := os.MkdirTemp("", "saige-plugin-")
	if err != nil {
		return "", err
	}
	plan := &exportPlan{}
	manifest, _ := json.MarshalIndent(map[string]any{"name": s.serverName(), "description": "Skills of the saige agent " + s.res.Name}, "", "  ")
	if err := plan.set(filepath.Join(dir, ".claude-plugin", "plugin.json"), append(manifest, '\n')); err != nil {
		return "", err
	}
	if err := s.planSkills(plan, filepath.Join(dir, "skills"), true); err != nil {
		return "", err
	}
	if err := plan.apply(); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// harnessExit carries a harness's exit status out of saige launch.
type harnessExit struct{ code int }

func (e harnessExit) Error() string { return fmt.Sprintf("harness exited with status %d", e.code) }
func (e harnessExit) ExitCode() int { return e.code }

// runHarness runs the harness attached to this terminal. The harness gets
// interrupts itself, from the terminal, so it is not tied to saige's
// context.
func runHarness(cmd *cobra.Command, l *launch) error {
	c := exec.Command(l.bin, l.args...) //nolint:gosec,noctx // the user asked to run this harness
	c.Stdin = os.Stdin
	c.Stdout = cmd.OutOrStdout()
	c.Stderr = cmd.ErrOrStderr()
	c.Env = append(os.Environ(), l.env...)
	err := c.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return reportedError{harnessExit{code: exitErr.ExitCode()}}
	}
	return err
}

// shellJoin quotes args for display.
func shellJoin(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		if a != "" && !strings.ContainsAny(a, " \t\n\"'\\$`{}[]*?;&|<>()#!") {
			out[i] = a
			continue
		}
		out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(out, " ")
}
