package source_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/urmzd/saige/rag/source"
	"github.com/urmzd/saige/rag/types"
)

// writeTree creates files (slash-separated paths relative to dir) with
// their own path as content.
func writeTree(t *testing.T, dir string, files ...string) {
	t.Helper()
	for _, f := range files {
		p := filepath.Join(dir, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// writeFile writes content to the slash-separated path rel below dir.
func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fetchRel runs FetchWithSkips and returns the fetched paths and the
// skipped paths with their reasons, all relative to dir with forward
// slashes. A skipped directory ends in "/".
func fetchRel(t *testing.T, s *source.Filesystem) (fetched []string, skipped map[string]string) {
	t.Helper()
	docs, skips, err := s.FetchWithSkips(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rel := func(p string) string {
		r, err := filepath.Rel(s.Dir, p)
		if err != nil {
			t.Fatal(err)
		}
		return filepath.ToSlash(r)
	}
	for _, d := range docs {
		fetched = append(fetched, rel(d.SourceURI))
	}
	slices.Sort(fetched)
	skipped = map[string]string{}
	for _, sk := range skips {
		r := rel(strings.TrimSuffix(sk.SourceURI, string(filepath.Separator)))
		if sk.Prefix {
			r += "/"
		}
		skipped[r] = sk.Reason
	}
	return fetched, skipped
}

func TestFilesystemDefaultsSkipToolDirsHiddenAndSecrets(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir,
		"README.md", "docs/guide.md",
		".git/config", ".git/HEAD", ".hg/store", ".svn/entries", "node_modules/pkg/index.js", "web/node_modules/x.js",
		".github/workflows/ci.yml", ".hidden.md", "docs/.draft.md",
		".env", ".env.local", "prod.env", "server.pem", "tls.key", "id_rsa", "id_rsa.pub", "id_ed25519",
		"cert.p12", "cert.pfx", "vault.kdbx", "credentials.json", "credentials-prod.json",
		"terraform.tfstate", "terraform.tfstate.backup", ".netrc", ".npmrc", ".pypirc", "docs/ID_RSA",
	)

	fetched, skipped := fetchRel(t, &source.Filesystem{Dir: dir, Recursive: true})
	if want := []string{"README.md", "docs/guide.md"}; !slices.Equal(fetched, want) {
		t.Fatalf("fetched %v, want %v", fetched, want)
	}
	for p, reason := range map[string]string{
		".git/": source.SkipToolDir, ".hg/": source.SkipToolDir, ".svn/": source.SkipToolDir,
		"node_modules/": source.SkipToolDir, "web/node_modules/": source.SkipToolDir,
		".github/": source.SkipHidden, ".hidden.md": source.SkipHidden, "docs/.draft.md": source.SkipHidden,
		".env": source.SkipHidden, "prod.env": source.SkipSecret, "server.pem": source.SkipSecret,
		"tls.key": source.SkipSecret, "id_rsa": source.SkipSecret, "id_rsa.pub": source.SkipSecret,
		"id_ed25519": source.SkipSecret, "cert.p12": source.SkipSecret, "cert.pfx": source.SkipSecret,
		"vault.kdbx": source.SkipSecret, "credentials.json": source.SkipSecret,
		"credentials-prod.json": source.SkipSecret, "terraform.tfstate": source.SkipSecret,
		"terraform.tfstate.backup": source.SkipSecret, "docs/ID_RSA": source.SkipSecret,
	} {
		if got := skipped[p]; got != reason {
			t.Errorf("skip reason for %s = %q, want %q", p, got, reason)
		}
	}
	if _, ok := skipped[".git/config"]; ok {
		t.Error("files under a skipped directory are reported one by one; the directory should cover them")
	}
}

func TestFilesystemIncludeHiddenStillDeniesSecretsAndToolDirs(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, ".github/workflows/ci.yml", ".hidden.md", ".git/config", ".env", ".env.local", ".netrc", ".npmrc", ".pypirc", "a.md")

	fetched, skipped := fetchRel(t, &source.Filesystem{Dir: dir, Recursive: true, IncludeHidden: true})
	if want := []string{".github/workflows/ci.yml", ".hidden.md", "a.md"}; !slices.Equal(fetched, want) {
		t.Fatalf("fetched %v, want %v", fetched, want)
	}
	for _, p := range []string{".env", ".env.local", ".netrc", ".npmrc", ".pypirc"} {
		if skipped[p] != source.SkipSecret {
			t.Errorf("%s: reason %q, want %q", p, skipped[p], source.SkipSecret)
		}
	}
	if skipped[".git/"] != source.SkipToolDir {
		t.Errorf(".git/: reason %q", skipped[".git/"])
	}
}

func TestFilesystemOverrides(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, "a.md", "server.pem", "node_modules/x.js", ".git/HEAD")

	tests := []struct {
		name string
		fs   source.Filesystem
		want []string
	}{
		{"allow secret names", source.Filesystem{AllowSecretNames: true}, []string{"a.md", "server.pem"}},
		{"tool dirs", source.Filesystem{IncludeToolDirs: true}, []string{"a.md", "node_modules/x.js"}},
		{"tool dirs and hidden", source.Filesystem{IncludeToolDirs: true, IncludeHidden: true}, []string{".git/HEAD", "a.md", "node_modules/x.js"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.fs
			s.Dir, s.Recursive = dir, true
			if got, _ := fetchRel(t, &s); !slices.Equal(got, tt.want) {
				t.Fatalf("fetched %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFilesystemIncludeExcludeGlobs(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, "README.md", "docs/a.md", "docs/deep/b.md", "docs/deep/c.txt", "build/out.md", "notes.txt")

	tests := []struct {
		name             string
		include, exclude []string
		want             []string
		reasons          map[string]string
	}{
		{
			name:    "include keeps only matches",
			include: []string{"docs/**/*.md"},
			want:    []string{"docs/a.md", "docs/deep/b.md"},
			reasons: map[string]string{"README.md": source.SkipNotIncluded, "docs/deep/c.txt": source.SkipNotIncluded},
		},
		{
			name:    "exclude skips a directory without descending",
			exclude: []string{"build/**", "*.txt"},
			want:    []string{"README.md", "docs/a.md", "docs/deep/b.md", "docs/deep/c.txt"},
			reasons: map[string]string{"build/": source.SkipExcluded, "notes.txt": source.SkipExcluded},
		},
		{
			name:    "exclude wins over include",
			include: []string{"**/*.md"},
			exclude: []string{"docs/deep"},
			want:    []string{"README.md", "build/out.md", "docs/a.md"},
			reasons: map[string]string{"docs/deep/": source.SkipExcluded},
		},
		{
			name:    "brace alternatives",
			include: []string{"{README.md,docs/*.md}"},
			want:    []string{"README.md", "docs/a.md"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, skipped := fetchRel(t, &source.Filesystem{Dir: dir, Recursive: true, Include: tt.include, Exclude: tt.exclude})
			if !slices.Equal(got, tt.want) {
				t.Fatalf("fetched %v, want %v", got, tt.want)
			}
			for p, reason := range tt.reasons {
				if skipped[p] != reason {
					t.Errorf("%s: reason %q, want %q (all: %v)", p, skipped[p], reason, skipped)
				}
			}
		})
	}
}

func TestFilesystemInvalidGlob(t *testing.T) {
	for _, s := range []*source.Filesystem{
		{Dir: t.TempDir(), Include: []string{"docs/[a"}},
		{Dir: t.TempDir(), Exclude: []string{"{a,b"}},
	} {
		if _, err := s.Fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "invalid") {
			t.Errorf("Fetch with a bad glob: err = %v", err)
		}
	}
}

func TestFilesystemExtensionsAndNonRecursiveAreReportedAsSkipped(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, "a.md", "b.png", "sub/c.md")
	got, skipped := fetchRel(t, &source.Filesystem{Dir: dir, Extensions: []string{".md"}})
	if !slices.Equal(got, []string{"a.md"}) {
		t.Fatalf("fetched %v", got)
	}
	if skipped["b.png"] != source.SkipExtension || skipped["sub/"] != source.SkipNotRecursive {
		t.Fatalf("skipped = %v", skipped)
	}
}

func TestFilesystemIgnoreFiles(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir,
		"README.md", "debug.log", "keep.log", "build/out.md", "src/build/gen.md",
		"src/main.md", "src/tmp/scratch.md", "docs/a.md", "docs/private/x.md", "docs/private/y.md",
		"vendor/lib/z.md", "vendor/lib/keep.md", "rooted.md", "src/rooted.md",
		"dironly/file.md", "src/dironly", "space .md",
		"#hash.md", "!bang.md", "contents/inner.md",
	)
	writeFile(t, dir, ".gitignore", strings.Join([]string{
		"# comment",
		"",
		"*.log",
		"!keep.log",
		"build/",     // directory only, any depth
		"/rooted.md", // anchored to the root
		"dironly/",   // matches the directory, not the file src/dironly
		"vendor/lib/*",
		"!vendor/lib/keep.md",
		"space\\ .md",
		"\\#hash.md",
		"\\!bang.md",
		"contents/**", // the contents, not the directory itself
		"!contents/inner.md",
		"trailing.md   ",
	}, "\n"))
	// A nested ignore file applies below its directory, and its rules beat
	// the parent's.
	writeFile(t, dir, "src/.gitignore", "tmp/\n!*.log\n")
	writeFile(t, dir, "src/kept.log", "log")
	writeFile(t, dir, "trailing.md", "t")
	// .saigeignore beats .gitignore in the same directory.
	writeFile(t, dir, "docs/.gitignore", "private/*\n")
	writeFile(t, dir, "docs/.saigeignore", "!private/y.md\n")

	got, skipped := fetchRel(t, &source.Filesystem{Dir: dir, Recursive: true})
	want := []string{
		"README.md", "contents/inner.md", "docs/a.md", "docs/private/y.md", "keep.log",
		"src/dironly", "src/kept.log", "src/main.md", "src/rooted.md", "vendor/lib/keep.md",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("fetched\n %v\nwant\n %v", got, want)
	}
	for _, p := range []string{
		"debug.log", "build/", "src/build/", "rooted.md", "dironly/", "vendor/lib/z.md", "space .md",
		"#hash.md", "!bang.md", "src/tmp/", "docs/private/x.md", "trailing.md",
	} {
		if skipped[p] != source.SkipIgnored {
			t.Errorf("%s: reason %q, want %q", p, skipped[p], source.SkipIgnored)
		}
	}

	// NoIgnoreFiles fetches everything that is not hidden.
	got, _ = fetchRel(t, &source.Filesystem{Dir: dir, Recursive: true, NoIgnoreFiles: true})
	if len(got) != 22 {
		t.Fatalf("NoIgnoreFiles fetched %d files: %v", len(got), got)
	}
}

func TestFilesystemIgnoredDirectoryCannotBeReincluded(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, "out/keep.md", "a.md")
	writeFile(t, dir, ".gitignore", "out/\n!out/keep.md\n")
	got, _ := fetchRel(t, &source.Filesystem{Dir: dir, Recursive: true})
	if !slices.Equal(got, []string{"a.md"}) {
		t.Fatalf("fetched %v; git does not re-include a file under an excluded directory", got)
	}
}

func TestFilesystemSkippedCoversURIs(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, "node_modules/a/b.js")
	_, skips, err := (&source.Filesystem{Dir: dir, Recursive: true}).FetchWithSkips(context.Background())
	if err != nil || len(skips) != 1 {
		t.Fatalf("skips = %v, %v", skips, err)
	}
	if !skips[0].Covers(filepath.Join(dir, "node_modules", "a", "b.js")) || skips[0].Covers(filepath.Join(dir, "node_modules_x")) {
		t.Fatalf("%+v covers the wrong URIs", skips[0])
	}
	var _ types.FilteringSource = &source.Filesystem{}
}
