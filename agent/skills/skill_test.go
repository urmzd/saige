package skills

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func skillMD(name, description string, extra ...string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\n" + strings.Join(extra, "\n") + "\n---\n# " + name + "\n\nInstructions.\n"
}

func TestParseSkillValidation(t *testing.T) {
	long := strings.Repeat("x", MaxDescriptionLength+1)
	tests := []struct {
		name    string
		in      string
		wantErr string
		check   func(*testing.T, SkillMeta)
	}{
		{name: "minimal", in: skillMD("pdf-tools", "Work with PDFs.")},
		{
			name: "allowed-tools as string",
			in:   skillMD("a", "d", "allowed-tools: read_file web_search"),
			check: func(t *testing.T, m SkillMeta) {
				if !slices.Equal(m.AllowedTools, []string{"read_file", "web_search"}) {
					t.Errorf("AllowedTools = %v", m.AllowedTools)
				}
			},
		},
		{
			name: "allowed-tools as list",
			in:   skillMD("a", "d", "allowed-tools: [read_file]"),
			check: func(t *testing.T, m SkillMeta) {
				if !slices.Equal(m.AllowedTools, []string{"read_file"}) {
					t.Errorf("AllowedTools = %v", m.AllowedTools)
				}
			},
		},
		{
			name: "allowed-tools empty narrows to nothing",
			in:   skillMD("a", "d", "allowed-tools: []"),
			check: func(t *testing.T, m SkillMeta) {
				if m.AllowedTools == nil || len(m.AllowedTools) != 0 {
					t.Errorf("AllowedTools = %#v, want empty non-nil", m.AllowedTools)
				}
			},
		},
		{
			name: "unknown keys are ignored",
			in:   skillMD("a", "d", "model: big", "user-invocable: true"),
		},
		{name: "missing name", in: "---\ndescription: d\n---\n", wantErr: "name is required"},
		{name: "missing description", in: "---\nname: a\n---\n", wantErr: "description is required"},
		{name: "uppercase name", in: skillMD("PDF", "d"), wantErr: "lowercase"},
		{name: "double hyphen", in: skillMD("a--b", "d"), wantErr: "lowercase"},
		{name: "leading hyphen", in: skillMD("-a", "d"), wantErr: "lowercase"},
		{name: "name too long", in: skillMD(strings.Repeat("a", MaxNameLength+1), "d"), wantErr: "longer than 64"},
		{name: "description too long", in: skillMD("a", long), wantErr: "longer than 1024"},
		{name: "metadata not a mapping", in: skillMD("a", "d", "metadata: [x]"), wantErr: "metadata must be a mapping"},
		{name: "name not a string", in: "---\nname: [a]\ndescription: d\n---\n", wantErr: "name must be a string"},
		{name: "not utf8", in: "---\nname: a\ndescription: \xff\n---\n", wantErr: "not UTF-8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta, _, err := ParseSkill([]byte(tt.in))
			if tt.wantErr != "" {
				if !errors.Is(err, ErrInvalidSkill) || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want ErrInvalidSkill containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tt.check != nil {
				tt.check(t, meta)
			}
		})
	}
}

func TestSnapshot(t *testing.T) {
	base := fstest.MapFS{
		"demo/SKILL.md":          {Data: []byte(skillMD("demo", "Demo skill."))},
		"demo/reference/api.md":  {Data: []byte("# API\n")},
		"demo/scripts/run.sh":    {Data: []byte("echo hi\n")},
		"wrong-dir/SKILL.md":     {Data: []byte(skillMD("other", "x"))},
		"empty/README.md":        {Data: []byte("no skill here")},
		"big/SKILL.md":           {Data: []byte(skillMD("big", "x"))},
		"big/blob.bin":           {Data: make([]byte, 64)},
		"many/SKILL.md":          {Data: []byte(skillMD("many", "x"))},
		"many/a":                 {Data: []byte("a")},
		"many/b":                 {Data: []byte("b")},
		"many/c":                 {Data: []byte("c")},
		"total/SKILL.md":         {Data: []byte(skillMD("total", "x"))},
		"total/a":                {Data: make([]byte, 40)},
		"total/b":                {Data: make([]byte, 40)},
		"demo2/SKILL.md":         {Data: []byte(skillMD("demo2", "Demo skill."))},
		"demo2/reference/api.md": {Data: []byte("# API\n")},
	}
	tests := []struct {
		name    string
		dir     string
		bounds  Bounds
		wantErr error
	}{
		{name: "valid package", dir: "demo"},
		{name: "name must match directory", dir: "wrong-dir", wantErr: ErrInvalidSkill},
		{name: "no SKILL.md", dir: "empty", wantErr: ErrInvalidSkill},
		{name: "file too large", dir: "big", bounds: Bounds{MaxFileBytes: 32}, wantErr: ErrBoundsExceeded},
		{name: "too many files", dir: "many", bounds: Bounds{MaxFiles: 3}, wantErr: ErrBoundsExceeded},
		{name: "total too large", dir: "total", bounds: Bounds{MaxTotalBytes: 100}, wantErr: ErrBoundsExceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := Snapshot(base, tt.dir, SnapshotOptions{Source: "test", Trusted: true, Bounds: tt.bounds})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			paths := make([]string, len(s.Resources))
			for i, r := range s.Resources {
				paths[i] = r.Path
			}
			if !slices.Equal(paths, []string{"reference/api.md", "scripts/run.sh"}) {
				t.Errorf("manifest = %v", paths)
			}
			if !strings.HasPrefix(s.Hash, "sha256:") || s.Source != "test" || !s.Trusted {
				t.Errorf("meta = %+v", s.SkillMeta)
			}
			if !strings.Contains(s.Body, "Instructions.") {
				t.Errorf("body = %q", s.Body)
			}
		})
	}

	t.Run("hash covers paths and contents", func(t *testing.T) {
		a, _ := Snapshot(base, "demo", SnapshotOptions{})
		renamed := fstest.MapFS{
			"demo/SKILL.md":       base["demo/SKILL.md"],
			"demo/reference/x.md": base["demo/reference/api.md"],
			"demo/scripts/run.sh": base["demo/scripts/run.sh"],
		}
		edited := fstest.MapFS{
			"demo/SKILL.md":         base["demo/SKILL.md"],
			"demo/reference/api.md": {Data: []byte("# API v2\n")},
			"demo/scripts/run.sh":   base["demo/scripts/run.sh"],
		}
		b, _ := Snapshot(renamed, "demo", SnapshotOptions{})
		c, _ := Snapshot(edited, "demo", SnapshotOptions{})
		again, _ := Snapshot(base, "demo", SnapshotOptions{})
		if a.Hash == b.Hash || a.Hash == c.Hash || a.Hash != again.Hash {
			t.Errorf("hashes: base=%s renamed=%s edited=%s again=%s", a.Hash, b.Hash, c.Hash, again.Hash)
		}
	})
}

func TestCleanResourcePath(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "reference/api.md", want: "reference/api.md"},
		{in: "./reference//api.md", want: "reference/api.md"},
		{in: "reference/../scripts/run.sh", want: "scripts/run.sh"},
		{in: "../secret", wantErr: true},
		{in: "reference/../../secret", wantErr: true},
		{in: "/etc/passwd", wantErr: true},
		{in: "..\\secret", wantErr: true},
		{in: ".", wantErr: true},
		{in: "", wantErr: true},
		{in: "a\x00b", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := CleanResourcePath(tt.in)
			if tt.wantErr {
				if !errors.Is(err, ErrOutsideSkillRoot) {
					t.Fatalf("err = %v, want ErrOutsideSkillRoot", err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("CleanResourcePath(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
			}
		})
	}
}

func TestSkillReadResource(t *testing.T) {
	fsys := fstest.MapFS{
		"demo/SKILL.md":         {Data: []byte(skillMD("demo", "d"))},
		"demo/reference/api.md": {Data: []byte("0123456789")},
	}
	s, err := Snapshot(fsys, "demo", SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		path    string
		max     int64
		want    string
		wantErr error
	}{
		{name: "listed file", path: "reference/api.md", want: "0123456789"},
		{name: "within limit", path: "reference/api.md", max: 10, want: "0123456789"},
		{name: "over limit", path: "reference/api.md", max: 9, wantErr: ErrResourceTooLarge},
		{name: "SKILL.md is not a resource", path: "SKILL.md", wantErr: ErrResourceNotFound},
		{name: "unlisted", path: "reference/other.md", wantErr: ErrResourceNotFound},
		{name: "escape", path: "../demo/SKILL.md", wantErr: ErrOutsideSkillRoot},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, _, err := s.ReadResource(tt.path, tt.max)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if err == nil && string(data) != tt.want {
				t.Errorf("data = %q", data)
			}
		})
	}

	t.Run("returned bytes are a copy", func(t *testing.T) {
		data, _, _ := s.ReadResource("reference/api.md", 0)
		data[0] = 'X'
		again, _, _ := s.ReadResource("reference/api.md", 0)
		if string(again) != "0123456789" {
			t.Fatalf("snapshot changed through a returned slice: %q", again)
		}
	})
	t.Run("tampered content fails the hash check", func(t *testing.T) {
		tampered := s
		tampered.files = map[string][]byte{"reference/api.md": []byte("tampered!!")}
		if _, _, err := tampered.ReadResource("reference/api.md", 0); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("err = %v, want ErrIntegrity", err)
		}
	})
}

func TestEffectiveTools(t *testing.T) {
	owner := []string{"read_file", "web_search", "write_file"}
	tests := []struct {
		name    string
		allowed []string
		want    []string
	}{
		{"field absent keeps all", nil, owner},
		{"intersection in owner order", []string{"write_file", "read_file"}, []string{"read_file", "write_file"}},
		{"cannot add a tool the agent lacks", []string{"exec_shell", "read_file"}, []string{"read_file"}},
		{"argument patterns match nothing", []string{"read_file(*.md)"}, []string{}},
		{"empty narrows to nothing", []string{}, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EffectiveTools(owner, SkillMeta{AllowedTools: tt.allowed})
			if !slices.Equal(got, tt.want) {
				t.Errorf("EffectiveTools = %v, want %v", got, tt.want)
			}
		})
	}
}

// repoSkills is the skills/ directory at the module root.
func repoSkills(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "skills"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("repository skills directory not found: %v", err)
	}
	return dir
}

func TestDirSourceRepositorySkills(t *testing.T) {
	src := NewDirSource(repoSkills(t), WithSourceName("repo"))
	skills, err := src.List(context.Background())
	if err != nil {
		t.Fatalf("repository skills must all validate: %v", err)
	}
	var names []string
	for _, s := range skills {
		names = append(names, s.Name)
		if s.Description == "" || s.Body == "" || !s.Trusted || s.Source != "repo" {
			t.Errorf("skill %s loaded incompletely: %+v", s.Name, s.SkillMeta)
		}
	}
	for _, want := range []string{"agent", "knowledge-graph", "rag"} {
		if !slices.Contains(names, want) {
			t.Errorf("skill %q missing from %v", want, names)
		}
	}
	for _, s := range skills {
		if s.Name == "agent" && s.Metadata["argument-hint"] != "[task]" {
			t.Errorf("agent metadata = %v", s.Metadata)
		}
	}

	fsSkills, err := NewFSSource("repo-fs", os.DirFS(repoSkills(t)), ".").List(context.Background())
	if err != nil || len(fsSkills) != len(skills) {
		t.Fatalf("FSSource listed %d skills (%v), DirSource %d", len(fsSkills), err, len(skills))
	}
	for i := range skills {
		if fsSkills[i].Hash != skills[i].Hash {
			t.Errorf("%s: hash differs between sources", skills[i].Name)
		}
	}
}

func writeSkill(t *testing.T, root, name string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(root, name)
	for p, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDirSourceConfinement(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, root, "good", map[string]string{"SKILL.md": skillMD("good", "Good."), "ref/a.md": "a"})
	writeSkill(t, root, "inside-link", map[string]string{"SKILL.md": skillMD("inside-link", "Link."), "real.md": "real"})
	if err := os.Symlink("real.md", filepath.Join(root, "inside-link", "alias.md")); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, root, "escape", map[string]string{"SKILL.md": skillMD("escape", "Escapes.")})
	if err := os.Symlink(secret, filepath.Join(root, "escape", "leak.txt")); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, root, "dirlink", map[string]string{"SKILL.md": skillMD("dirlink", "Dir link.")})
	if err := os.Symlink(outside, filepath.Join(root, "dirlink", "outside")); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, root, "broken", map[string]string{"SKILL.md": "no frontmatter"})

	skills, err := NewDirSource(root, WithSourceName("tmp")).List(context.Background())
	var names []string
	for _, s := range skills {
		names = append(names, s.Name)
	}
	if !slices.Equal(names, []string{"good", "inside-link"}) {
		t.Errorf("listed %v, want good and inside-link", names)
	}
	for _, s := range skills {
		if s.Name == "inside-link" {
			if _, ok := s.Resource("alias.md"); !ok {
				t.Error("a link inside the package should be followed")
			}
		}
	}
	var failed []string
	for _, e := range unwrapAll(err) {
		var se *SkillError
		if errors.As(e, &se) {
			failed = append(failed, se.Dir)
		}
	}
	slices.Sort(failed)
	if !slices.Equal(failed, []string{"broken", "dirlink", "escape"}) {
		t.Errorf("failed packages = %v (%v)", failed, err)
	}

	t.Run("FSSource rejects links", func(t *testing.T) {
		_, err := NewFSSource("dirfs", os.DirFS(root), ".").List(context.Background())
		if err == nil || !strings.Contains(err.Error(), "inside-link") {
			t.Errorf("err = %v, want inside-link rejected", err)
		}
	})
	t.Run("missing root lists nothing", func(t *testing.T) {
		got, err := NewDirSource(filepath.Join(root, "nope")).List(context.Background())
		if err != nil || got != nil {
			t.Errorf("got %v, %v", got, err)
		}
	})
}

func unwrapAll(err error) []error {
	if err == nil {
		return nil
	}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		return j.Unwrap()
	}
	return []error{err}
}
