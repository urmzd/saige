package skills

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCursorRule(t *testing.T) {
	tests := []struct {
		name     string
		rel      string
		in       string
		wantName string
		wantDesc string
		wantBody string
		wantMeta map[string]string
		wantErr  error
	}{
		{
			name:     "description and unquoted globs",
			rel:      ".cursor/rules/go.mdc",
			in:       "---\ndescription: Go style\nglobs: *.go, **/*_test.go\nalwaysApply: false\n---\n\nUse table tests.\n",
			wantName: "go",
			wantDesc: "Go style. Cursor applies it to files matching *.go, **/*_test.go.",
			wantBody: "Use table tests.\n",
			wantMeta: map[string]string{MetaCursorRule: ".cursor/rules/go.mdc", MetaCursorGlobs: "*.go,**/*_test.go"},
		},
		{
			name:     "flow list globs",
			rel:      ".cursor/rules/ts.mdc",
			in:       "---\ndescription: 'TS rules'\nglobs: [\"*.ts\", '*.tsx']\n---\nbody\n",
			wantName: "ts",
			wantDesc: "TS rules. Cursor applies it to files matching *.ts, *.tsx.",
			wantBody: "body\n",
			wantMeta: map[string]string{MetaCursorRule: ".cursor/rules/ts.mdc", MetaCursorGlobs: "*.ts,*.tsx"},
		},
		{
			name:     "always apply without description",
			rel:      ".cursor/rules/Security Rules.mdc",
			in:       "---\ndescription:\nglobs:\nalwaysApply: true\n---\nNo secrets.\n",
			wantName: "security-rules",
			wantDesc: "Cursor rule .cursor/rules/Security Rules.mdc. Cursor applies it to every request: load it before starting work.",
			wantBody: "No secrets.\n",
			wantMeta: map[string]string{MetaCursorRule: ".cursor/rules/Security Rules.mdc", MetaCursorAlwaysApply: "true"},
		},
		{
			name:     "nested path joins with hyphens",
			rel:      ".cursor/rules/web/api/routes.mdc",
			in:       "---\ndescription: Routes!\n---\nx",
			wantName: "web-api-routes",
			wantDesc: "Routes!",
			wantBody: "x",
			wantMeta: map[string]string{MetaCursorRule: ".cursor/rules/web/api/routes.mdc"},
		},
		{
			name:     "no header",
			rel:      ".cursor/rules/plain.mdc",
			in:       "\r\nJust text.\r\n",
			wantName: "plain",
			wantDesc: "Cursor rule .cursor/rules/plain.mdc.",
			wantBody: "Just text.\n",
			wantMeta: map[string]string{MetaCursorRule: ".cursor/rules/plain.mdc"},
		},
		{
			name:     "unclosed header is body",
			rel:      ".cursor/rules/open.mdc",
			in:       "---\ndescription: x\nbody",
			wantName: "open",
			wantDesc: "Cursor rule .cursor/rules/open.mdc.",
			wantBody: "---\ndescription: x\nbody",
			wantMeta: map[string]string{MetaCursorRule: ".cursor/rules/open.mdc"},
		},
		{
			name:     "legacy file always applies",
			rel:      ".cursorrules",
			in:       "Be concise.\n",
			wantName: "cursorrules",
			wantDesc: "Cursor rule .cursorrules. Cursor applies it to every request: load it before starting work.",
			wantBody: "Be concise.\n",
			wantMeta: map[string]string{MetaCursorRule: ".cursorrules", MetaCursorAlwaysApply: "true"},
		},
		{name: "no usable name", rel: ".cursor/rules/___.mdc", in: "x", wantErr: ErrInvalidSkill},
		{name: "outside the rules directory", rel: "docs/x.mdc", in: "x", wantErr: ErrInvalidSkill},
		{
			name:    "description too long",
			rel:     ".cursor/rules/long.mdc",
			in:      "---\ndescription: " + strings.Repeat("x", MaxDescriptionLength) + "\n---\n",
			wantErr: ErrInvalidSkill,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sk, err := CursorRule(tt.rel, []byte(tt.in), SnapshotOptions{Source: "src", Trusted: true})
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if sk.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", sk.Name, tt.wantName)
			}
			if sk.Description != tt.wantDesc {
				t.Errorf("Description = %q, want %q", sk.Description, tt.wantDesc)
			}
			if sk.Body != tt.wantBody {
				t.Errorf("Body = %q, want %q", sk.Body, tt.wantBody)
			}
			if !mapsEqual(sk.Metadata, tt.wantMeta) {
				t.Errorf("Metadata = %v, want %v", sk.Metadata, tt.wantMeta)
			}
			if sk.Source != "src" || !sk.Trusted || !strings.HasPrefix(sk.Hash, "sha256:") || len(sk.Resources) != 0 {
				t.Errorf("snapshot fields = %+v", sk.SkillMeta)
			}
		})
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

func TestCursorRuleHashTracksFile(t *testing.T) {
	opts := SnapshotOptions{Source: "s", Trusted: true}
	a, _ := CursorRule(".cursor/rules/a.mdc", []byte("one"), opts)
	b, _ := CursorRule(".cursor/rules/a.mdc", []byte("one"), opts)
	c, _ := CursorRule(".cursor/rules/a.mdc", []byte("two"), opts)
	if a.Hash != b.Hash || a.Hash == c.Hash {
		t.Errorf("hashes: same content %q %q, different content %q", a.Hash, b.Hash, c.Hash)
	}
}

func TestCursorRulesSourceFixture(t *testing.T) {
	src := NewCursorRulesSource(filepath.Join("testdata", "cursor-project"), Untrusted())
	got, err := src.List(context.Background())
	var se *SkillError
	if !errors.As(err, &se) || se.Dir != ".cursor/rules/___.mdc" {
		t.Errorf("err = %v, want a SkillError for ___.mdc", err)
	}
	var names []string
	for _, s := range got {
		names = append(names, s.Name)
		if s.Trusted {
			t.Errorf("%s is trusted from an untrusted source", s.Name)
		}
	}
	want := []string{"always", "frontend-react-components", "go-style", "manual", "cursorrules"}
	if !slices.Equal(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
	react := got[1]
	if react.Metadata[MetaCursorGlobs] != "web/**/*.tsx,web/**/*.ts" {
		t.Errorf("react globs = %q", react.Metadata[MetaCursorGlobs])
	}
}

func TestCursorRulesSourceMissing(t *testing.T) {
	for _, dir := range []string{t.TempDir(), filepath.Join(t.TempDir(), "absent")} {
		got, err := NewCursorRulesSource(dir).List(context.Background())
		if err != nil || len(got) != 0 {
			t.Errorf("%s: got %v, %v; want nothing", dir, got, err)
		}
	}
}

func TestCursorRulesSourceRejectsEscapingLink(t *testing.T) {
	project := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.mdc")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	rules := filepath.Join(project, ".cursor", "rules")
	if err := os.MkdirAll(rules, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(rules, "leak.mdc")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	got, err := NewCursorRulesSource(project).List(context.Background())
	if len(got) != 0 || !errors.Is(err, ErrOutsideSkillRoot) {
		t.Errorf("got %v, %v; want ErrOutsideSkillRoot", got, err)
	}
}

func TestStandardSources(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(project, ".agents/skills/shared/SKILL.md"), skillMD("shared", "From the project."))
	write(filepath.Join(project, ".claude/skills/claude-proj/SKILL.md"), skillMD("claude-proj", "Claude project skill."))
	write(filepath.Join(project, ".cursor/rules/style.mdc"), "---\ndescription: Style\n---\nx\n")
	write(filepath.Join(project, ".cursorrules"), "legacy\n")
	write(filepath.Join(home, ".agents/skills/shared/SKILL.md"), skillMD("shared", "From home."))
	write(filepath.Join(home, ".claude/skills/claude-user/SKILL.md"), skillMD("claude-user", "Claude user skill."))

	tests := []struct {
		name          string
		project, home string
		want          []string
		untrusted     []string
	}{
		{
			name: "project and home", project: project, home: home,
			want: []string{"claude-proj", "claude-user", "cursorrules", "shared", "style"},
			// The trusted home definition of shared replaces the untrusted
			// project one.
			untrusted: []string{"claude-proj", "cursorrules", "style"},
		},
		{name: "home only", home: home, want: []string{"claude-user", "shared"}},
		{
			name: "project only", project: project,
			want:      []string{"claude-proj", "cursorrules", "shared", "style"},
			untrusted: []string{"claude-proj", "cursorrules", "shared", "style"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cat, err := NewCatalog(context.Background(), StandardSources(tt.project, tt.home, Untrusted()))
			if err != nil {
				t.Fatal(err)
			}
			if p := cat.Problems(); len(p) != 0 {
				t.Fatalf("problems: %v", p)
			}
			metas, err := cat.List(context.Background(), "")
			if err != nil {
				t.Fatal(err)
			}
			var names, untrusted []string
			for _, m := range metas {
				names = append(names, m.Name)
				if !m.Trusted {
					untrusted = append(untrusted, m.Name)
				}
			}
			if !slices.Equal(names, tt.want) {
				t.Errorf("names = %v, want %v", names, tt.want)
			}
			if !slices.Equal(untrusted, tt.untrusted) {
				t.Errorf("untrusted = %v, want %v", untrusted, tt.untrusted)
			}
		})
	}
}
