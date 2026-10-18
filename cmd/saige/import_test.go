package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/skills"
)

// importDir holds the import fixtures, resolved before any test changes
// the working directory.
var importDir, _ = filepath.Abs(filepath.Join("testdata", "import"))

// importSandbox copies the fixture project into a temporary directory, with
// HOME isolated, and returns the copy.
func importSandbox(t *testing.T) string {
	t.Helper()
	home, _ := catalogSandbox(t)
	t.Setenv("CODEX_HOME", "")
	project := filepath.Join(home, "project")
	if err := os.CopyFS(project, os.DirFS(filepath.Join(importDir, "project"))); err != nil {
		t.Fatal(err)
	}
	return project
}

// skillTree renders every file under dir/.agents/skills.
func skillTree(t *testing.T, dir string) string {
	t.Helper()
	root := filepath.Join(dir, ".agents", "skills")
	if _, err := os.Stat(root); err != nil {
		return ""
	}
	return snapshotFiles(t, root)
}

func TestImportGolden(t *testing.T) {
	project := importSandbox(t)
	code, out := runCLI(t, "import", "--dir", project)
	if code != 0 {
		t.Fatalf("import: %d %s", code, out)
	}
	checkGolden(t, filepath.Join(importDir, "files.golden"), skillTree(t, project))
	checkGolden(t, filepath.Join(importDir, "import.out"), normalizePaths(out, project))
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(project, ".agents", "skills", "review", "scripts", "check.sh"))
		if err != nil || info.Mode().Perm()&0o100 == 0 {
			t.Errorf("script mode = %v, %v; want executable", info.Mode(), err)
		}
	}

	// What import wrote loads as skills.
	loaded, err := skills.NewDirSource(filepath.Join(project, ".agents", "skills")).List(context.Background())
	if err != nil || len(loaded) != 5 {
		t.Fatalf("imported skills: %d loaded, %v", len(loaded), err)
	}

	// A second import changes nothing.
	before := skillTree(t, project)
	code, out = runCLI(t, "import", "--dir", project)
	if code != 0 || strings.Contains(out, "create") || strings.Contains(out, "update") {
		t.Fatalf("second import: %d %s", code, out)
	}
	if after := skillTree(t, project); after != before {
		t.Fatalf("second import changed files:\n%s", unifiedDiff("files", []byte(before), []byte(after)))
	}
}

func TestImportCases(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		setup    func(t *testing.T, project string)
		wantCode int
		wantOut  []string
		// wantFiles maps a path under .agents/skills to a substring it must
		// hold, or "" when it must not exist.
		wantFiles map[string]string
	}{
		{
			name:      "dry run writes nothing",
			args:      []string{"--dry-run"},
			wantOut:   []string{"would create     .agents/skills/review/SKILL.md", "+name: review"},
			wantFiles: map[string]string{"review/SKILL.md": "", "lint/SKILL.md": ""},
		},
		{
			name:      "one format",
			args:      []string{"cursor"},
			wantOut:   []string{"create           .agents/skills/web-components/SKILL.md", "create           .agents/skills/cursorrules/SKILL.md"},
			wantFiles: map[string]string{"web-components/SKILL.md": "cursor-globs: web/**/*.tsx", "review/SKILL.md": "", "lint/SKILL.md": ""},
		},
		{
			name:      "force replaces a hand-written skill",
			args:      []string{"claude", "--force"},
			wantOut:   []string{"update           .agents/skills/hand/SKILL.md"},
			wantFiles: map[string]string{"hand/SKILL.md": "Claude copy."},
		},
		{
			name:      "hand-written skill is kept",
			args:      []string{"claude"},
			wantOut:   []string{"kept .agents/skills/hand", "--force"},
			wantFiles: map[string]string{"hand/SKILL.md": "Keep me."},
		},
		{
			name: "source edits update an imported skill",
			setup: func(t *testing.T, project string) {
				if code, out := runCLI(t, "import", "--dir", project); code != 0 {
					t.Fatalf("first import: %d %s", code, out)
				}
				writeFile(t, filepath.Join(project, ".cursorrules"), "Be brief.\n")
			},
			wantOut:   []string{"update           .agents/skills/cursorrules/SKILL.md", "unchanged        .agents/skills/lint/SKILL.md"},
			wantFiles: map[string]string{"cursorrules/SKILL.md": "Be brief."},
		},
		{
			name: "linked directory is already canonical",
			args: []string{"claude"},
			setup: func(t *testing.T, project string) {
				if runtime.GOOS == "windows" {
					t.Skip("symlinks need privileges on Windows")
				}
				claude := filepath.Join(project, ".claude", "skills")
				if err := os.RemoveAll(claude); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(project, ".agents", "skills"), claude); err != nil {
					t.Fatal(err)
				}
			},
			wantFiles: map[string]string{"hand/SKILL.md": "Keep me."},
		},
		{
			name:     "unknown format",
			args:     []string{"vim"},
			wantCode: exitInvalid,
			wantOut:  []string{`unknown format "vim"`},
		},
		{
			name: "nothing to import",
			setup: func(t *testing.T, project string) {
				for _, d := range []string{".claude", ".codex", ".cursor", ".cursorrules"} {
					if err := os.RemoveAll(filepath.Join(project, d)); err != nil {
						t.Fatal(err)
					}
				}
			},
			wantOut: []string{"note: no skills or rules found for claude, codex, cursor"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			project := importSandbox(t)
			if tt.setup != nil {
				tt.setup(t, project)
			}
			code, out := runCLI(t, append([]string{"import", "--dir", project}, tt.args...)...)
			if code != tt.wantCode {
				t.Fatalf("exit %d, want %d: %s", code, tt.wantCode, out)
			}
			for _, w := range tt.wantOut {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q:\n%s", w, out)
				}
			}
			for rel, want := range tt.wantFiles {
				raw, err := os.ReadFile(filepath.Join(project, ".agents", "skills", filepath.FromSlash(rel)))
				switch {
				case want == "" && err == nil:
					t.Errorf("%s exists:\n%s", rel, raw)
				case want != "" && (err != nil || !strings.Contains(string(raw), want)):
					t.Errorf("%s lacks %q: %v\n%s", rel, want, err, raw)
				}
			}
		})
	}
}

func TestImportUser(t *testing.T) {
	project := importSandbox(t)
	home := filepath.Dir(project)
	writeFile(t, filepath.Join(home, ".claude", "skills", "mine", "SKILL.md"), "---\nname: mine\ndescription: User skill.\n---\nMine.\n")
	codexHome := filepath.Join(home, "codex-home")
	t.Setenv("CODEX_HOME", codexHome)
	writeFile(t, filepath.Join(codexHome, "skills", "theirs", "SKILL.md"), "---\nname: theirs\ndescription: Codex user skill.\n---\nTheirs.\n")

	code, out := runCLI(t, "import", "--user")
	if code != 0 {
		t.Fatalf("import --user: %d %s", code, out)
	}
	for _, w := range []string{"create           .agents/skills/mine/SKILL.md", "create           .agents/skills/theirs/SKILL.md", "Cursor keeps user rules in its settings"} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, "review") {
		t.Errorf("--user read the project:\n%s", out)
	}
}
