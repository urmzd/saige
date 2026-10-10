package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// exportSandbox writes a definition with harness tools, a skill with a
// resource, and limits, and returns the project and the agents directory.
func exportSandbox(t *testing.T) (project, agents string) {
	t.Helper()
	_, project = agentsSandbox(t)
	agents = filepath.Join(project, "agents")
	writeFile(t, filepath.Join(agents, "helper.agent.md"), definitionFile("helper", "1.2.0",
		"description: Answers questions about the project and edits files.\n"+
			"model: anthropic/claude-haiku-5-5\n"+
			"tools:\n  harness: [read, write]\n"+
			"skills: [tidy]\n"+
			"limits:\n  max_iterations: 8\n"+
			"approval:\n  grant: tool\n",
		"You are a careful helper.\nAnswer briefly."))
	// The skill lives where the CLI's skill catalog looks: the working
	// directory's .agents/skills.
	cwd, _ := os.Getwd()
	writeFile(t, filepath.Join(cwd, ".agents", "skills", "tidy", "SKILL.md"),
		"---\nname: tidy\ndescription: Tidy up text files.\nlicense: MIT\ndisable-model-invocation: true\nmetadata:\n  owner: docs\n---\nRemove trailing whitespace.\n")
	writeFile(t, filepath.Join(cwd, ".agents", "skills", "tidy", "scripts", "tidy.sh"), "#!/bin/sh\nsed -i 's/ *$//' \"$1\"\n")
	return project, agents
}

func exportArgs(harness, project, agents string, extra ...string) []string {
	return append([]string{"export", harness, "--agent", "helper", "--agents-dir", agents, "--dir", project}, extra...)
}

// snapshotFiles renders every file under root that export may write,
// skipping the sandbox's own inputs, with root shown as $PROJECT.
func snapshotFiles(t *testing.T, root string) string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			if rel == "agents" || rel == "sub" || rel == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	var b strings.Builder
	for _, p := range paths {
		raw, err := os.ReadFile(filepath.Join(root, p))
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString("== " + filepath.ToSlash(p) + " ==\n" + string(raw))
	}
	return normalizePaths(b.String(), root)
}

func normalizePaths(s, project string) string {
	real, _ := filepath.EvalSymlinks(project)
	for _, p := range []string{real, project} {
		if p != "" {
			s = strings.ReplaceAll(s, p, "$PROJECT")
		}
	}
	return s
}

// goldenDir is resolved before any test changes the working directory.
var goldenDir, _ = filepath.Abs(filepath.Join("testdata", "export"))

func TestExportGolden(t *testing.T) {
	for _, harness := range exportHarnesses {
		t.Run(harness, func(t *testing.T) {
			project, agents := exportSandbox(t)
			code, out := runCLI(t, exportArgs(harness, project, agents)...)
			if code != 0 {
				t.Fatalf("export: %d %s", code, out)
			}
			checkGolden(t, filepath.Join(goldenDir, harness+".golden"), snapshotFiles(t, project))
			checkGolden(t, filepath.Join(goldenDir, harness+".out"), normalizePaths(out, project))

			// A second export changes nothing.
			before := snapshotFiles(t, project)
			code, out = runCLI(t, exportArgs(harness, project, agents)...)
			if code != 0 || strings.Contains(out, "create") || strings.Contains(out, "update") {
				t.Fatalf("second export: %d %s", code, out)
			}
			if after := snapshotFiles(t, project); after != before {
				t.Fatalf("second export changed files:\n%s", unifiedDiff("files", []byte(before), []byte(after)))
			}
		})
	}
}

func TestExportMergesExistingConfig(t *testing.T) {
	tests := []struct {
		harness, path, existing string
		want                    []string
	}{
		{harnessClaude, ".mcp.json",
			"{\n    \"mcpServers\": {\n        \"other\": {\"command\": \"other-mcp\"}\n    },\n    \"zeta\": true\n}\n",
			[]string{"\n    \"mcpServers\": {\n        \"other\": {\n            \"command\": \"other-mcp\"\n        },\n        \"saige-helper\": {", "\"zeta\": true"}},
		{harnessGemini, ".gemini/settings.json",
			`{"theme": "dark", "mcpServers": {"saige-helper": {"command": "stale"}}}`,
			[]string{`"theme": "dark"`, `"command": "saige-mcp"`}},
		{harnessOpencode, "opencode.json",
			`{"$schema": "https://opencode.ai/config.json", "model": "anthropic/claude-haiku-5-5", "mcp": {"local": {"type": "local", "command": ["x"]}}}`,
			[]string{`"model": "anthropic/claude-haiku-5-5"`, `"local": {`, `"saige-helper": {`}},
		{harnessCursor, ".cursor/mcp.json",
			`{"mcpServers": {"github": {"url": "https://example.test/mcp"}}}`,
			[]string{`"github": {`, `"saige-helper": {`}},
		{harnessCodex, ".codex/config.toml",
			"# my settings\nmodel = \"gpt-6-luna\"\n\n[mcp_servers.saige-helper]\ncommand = \"stale\"\n\n[mcp_servers.saige-helper.env]\nX = \"1\"\n\n[mcp_servers.other]\ncommand = \"other\" # keep\n",
			[]string{"# my settings\nmodel = \"gpt-6-luna\"\n\n[mcp_servers.saige-helper]\ncommand = \"saige-mcp\"", "env_vars = [", "\n\n[mcp_servers.other]\ncommand = \"other\" # keep\n"}},
	}
	for _, tt := range tests {
		t.Run(tt.harness, func(t *testing.T) {
			project, agents := exportSandbox(t)
			path := writeFile(t, filepath.Join(project, tt.path), tt.existing)
			code, out := runCLI(t, exportArgs(tt.harness, project, agents)...)
			if code != 0 {
				t.Fatalf("export: %d %s", code, out)
			}
			raw, _ := os.ReadFile(path)
			for _, w := range tt.want {
				if !strings.Contains(string(raw), w) {
					t.Fatalf("%s lacks %q:\n%s", tt.path, w, raw)
				}
			}
			if strings.Contains(string(raw), "stale") || strings.Contains(string(raw), "X = ") {
				t.Fatalf("the old saige entry was kept:\n%s", raw)
			}
			// Merging again is a no-op.
			if code, out := runCLI(t, exportArgs(tt.harness, project, agents)...); code != 0 || !strings.Contains(out, "unchanged        "+tt.path) {
				t.Fatalf("second export: %d %s", code, out)
			}
		})
	}
}

func TestExportRefusals(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, project string) []string
		want  string
	}{
		{"malformed JSON", func(t *testing.T, project string) []string {
			writeFile(t, filepath.Join(project, ".mcp.json"), "{not json")
			return []string{harnessClaude}
		}, ".mcp.json"},
		{"codex dotted parent key", func(t *testing.T, project string) []string {
			writeFile(t, filepath.Join(project, ".codex", "config.toml"), "mcp_servers.other.command = \"x\"\n")
			return []string{harnessCodex}
		}, "add the server by hand"},
		{"skill saige did not write", func(t *testing.T, project string) []string {
			writeFile(t, filepath.Join(project, ".claude", "skills", "tidy", "SKILL.md"), "---\nname: tidy\ndescription: mine\n---\nmine\n")
			return []string{harnessClaude}
		}, "--force"},
		{"unknown harness", func(*testing.T, string) []string { return []string{"vim"} }, "unknown harness"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			project, agents := exportSandbox(t)
			args := tt.setup(t, project)
			code, out := runCLI(t, exportArgs(args[0], project, agents)...)
			if code != exitInvalid || !strings.Contains(out, tt.want) {
				t.Fatalf("got %d %s, want exit %d with %q", code, out, exitInvalid, tt.want)
			}
		})
	}
}

func TestExportForceReplacesASkill(t *testing.T) {
	project, agents := exportSandbox(t)
	skill := writeFile(t, filepath.Join(project, ".claude", "skills", "tidy", "SKILL.md"), "---\nname: tidy\ndescription: mine\n---\nmine\n")
	if code, out := runCLI(t, exportArgs(harnessClaude, project, agents, "--force")...); code != 0 {
		t.Fatalf("export --force: %d %s", code, out)
	}
	raw, _ := os.ReadFile(skill)
	if !strings.Contains(string(raw), "generated-by: saige export") {
		t.Fatalf("skill = %s", raw)
	}
}

func TestExportRefusesAnUntrustedDefinition(t *testing.T) {
	_, project := agentsSandbox(t)
	writeFile(t, filepath.Join(project, ".saige", "agents", "helper.agent.md"), definitionFile("helper", "1.0.0", "", "help"))
	code, out := runCLI(t, "export", harnessClaude, "--agent", "helper", "--dir", project)
	if code != exitInvalid || !strings.Contains(out, "untrusted") {
		t.Fatalf("got %d %s", code, out)
	}
}

func TestExportDryRunWritesNothing(t *testing.T) {
	project, agents := exportSandbox(t)
	code, out := runCLI(t, exportArgs(harnessCodex, project, agents, "--dry-run")...)
	if code != 0 || !strings.Contains(out, "would create     .codex/config.toml") || !strings.Contains(out, "+[mcp_servers.saige-helper]") {
		t.Fatalf("dry run: %d %s", code, out)
	}
	if got := snapshotFiles(t, project); got != "" {
		t.Fatalf("dry run wrote files:\n%s", got)
	}
}

func TestExportUserLevelPrintsTheDiff(t *testing.T) {
	project, agents := exportSandbox(t)
	home := os.Getenv("HOME")
	writeFile(t, filepath.Join(home, ".claude.json"), "{\n  \"numStartups\": 3,\n  \"mcpServers\": {}\n}\n")
	code, out := runCLI(t, exportArgs(harnessClaude, project, agents, "--user")...)
	if code != 0 {
		t.Fatalf("export --user: %d %s", code, out)
	}
	for _, want := range []string{"update           .claude.json", "+    \"saige-helper\": {", "create           .claude/agents/helper.md"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(home, ".claude.json"))
	if !strings.Contains(string(raw), `"numStartups": 3`) || !strings.Contains(string(raw), `"--root",`) || !strings.Contains(string(raw), `"."`) {
		t.Fatalf(".claude.json = %s", raw)
	}
	// Nothing went to the project.
	if got := snapshotFiles(t, project); got != "" {
		t.Fatalf("--user wrote to the project:\n%s", got)
	}
}

// fakeHarness writes a script that records its arguments and the
// OPENCODE_CONFIG_CONTENT it got, then exits with status 3.
func fakeHarness(t *testing.T) (bin, record string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake harness is a shell script")
	}
	dir := t.TempDir()
	record = filepath.Join(dir, "record")
	bin = filepath.Join(dir, "harness")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > " + record + "\n" +
		"printf 'ENV=%s\\n' \"$OPENCODE_CONFIG_CONTENT\" >> " + record + "\n" +
		"ls \"$2\" >/dev/null 2>&1\nexit 3\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { //nolint:gosec // the test's own script
		t.Fatal(err)
	}
	return bin, record
}

func TestLaunch(t *testing.T) {
	tests := []struct {
		harness string
		want    []string
		// files the launch writes to the project; nil writes none.
		files []string
	}{
		{harnessClaude, []string{"--mcp-config", `"saige-helper":{"type":"stdio","command":"saige-mcp"`, "--agents", `"helper":{`, "--plugin-dir", "-p", "hi"}, nil},
		{harnessCodex, []string{"-c", `mcp_servers.saige-helper.command="saige-mcp"`, `mcp_servers.saige-helper.args=["--agents-dir"`, "-p", "hi"}, []string{".agents/skills/tidy/SKILL.md"}},
		{harnessOpencode, []string{`ENV={`, `"saige-helper":{"type":"local","command":["saige-mcp","--agents-dir"`, `"helper":{`, `"prompt":"You are a careful helper.`}, []string{".agents/skills/tidy/SKILL.md"}},
		{harnessGemini, []string{"-p", "hi"}, []string{".gemini/settings.json", ".agents/skills/tidy/SKILL.md"}},
		{harnessCursor, []string{"-p", "hi"}, []string{".cursor/mcp.json", ".agents/skills/tidy/SKILL.md"}},
	}
	for _, tt := range tests {
		t.Run(tt.harness, func(t *testing.T) {
			project, agents := exportSandbox(t)
			bin, record := fakeHarness(t)
			t.Setenv("OPENCODE_CONFIG_CONTENT", "")
			args := []string{"launch", tt.harness, "--agent", "helper", "--agents-dir", agents, "--dir", project, "--bin", bin, "--", "-p", "hi"}

			code, out := runCLI(t, append(args[:len(args)-3:len(args)-3], "--dry-run", "--", "-p", "hi")...)
			if code != 0 || !strings.Contains(out, "would run "+bin) {
				t.Fatalf("dry run: %d %s", code, out)
			}
			if got := snapshotFiles(t, project); got != "" {
				t.Fatalf("dry run wrote files:\n%s", got)
			}

			code, out = runCLI(t, args...)
			if code != 3 {
				t.Fatalf("launch exit = %d, want the harness's 3: %s", code, out)
			}
			raw, _ := os.ReadFile(record)
			for _, w := range tt.want {
				if !strings.Contains(string(raw), w) {
					t.Fatalf("harness got no %q:\n%s", w, raw)
				}
			}
			got := snapshotFiles(t, project)
			for _, f := range tt.files {
				if !strings.Contains(got, "== "+f+" ==") {
					t.Fatalf("launch did not write %s:\n%s", f, got)
				}
			}
			if tt.files == nil && got != "" {
				t.Fatalf("launch wrote files:\n%s", got)
			}
		})
	}
}
