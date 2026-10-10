package definition

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func ids(defs []*Definition) []string {
	var out []string
	for _, d := range defs {
		out = append(out, d.ID())
	}
	slices.Sort(out)
	return out
}

func TestDirSource(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.agent.md"), minimal("a", "1.0.0"))
	writeFile(t, filepath.Join(dir, "team", "deep", "b.agent.md"), minimal("b", "1.0.0"))
	writeFile(t, filepath.Join(dir, "team", "agents", "c.md"), minimal("c", "2.0.0"))
	writeFile(t, filepath.Join(dir, "README.md"), "# not a definition")
	writeFile(t, filepath.Join(dir, "team", "notes.md"), "not one either")
	defs, err := DirSource(dir).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(defs); !reflect.DeepEqual(got, []string{"a@1.0.0", "b@1.0.0", "c@2.0.0"}) {
		t.Fatalf("got %v", got)
	}
	for _, d := range defs {
		if d.Source != dir || !d.Trusted || d.Path == "" {
			t.Fatalf("definition %s: source %q path %q trusted %v", d.ID(), d.Source, d.Path, d.Trusted)
		}
	}
}

func TestDirSourceNamedAgents(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agents")
	writeFile(t, filepath.Join(dir, "x.md"), minimal("x", "1.0.0"))
	defs, err := DirSource(dir).Load(context.Background())
	if err != nil || len(defs) != 1 {
		t.Fatalf("got %v %v", ids(defs), err)
	}
}

func TestDirSourceMissingAndBroken(t *testing.T) {
	defs, err := DirSource(filepath.Join(t.TempDir(), "absent")).Load(context.Background())
	if err != nil || len(defs) != 0 {
		t.Fatalf("missing directory: %v %v", defs, err)
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "good.agent.md"), minimal("good", "1.0.0"))
	writeFile(t, filepath.Join(dir, "bad.agent.md"), header("bogus: 1\n"))
	_, err = DirSource(dir).Load(context.Background())
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "bad.agent.md") || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("got %v", err)
	}
}

func TestDirSourceRefusesEscapingLink(t *testing.T) {
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret.agent.md"), minimal("secret", "1.0.0"))
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(outside, "secret.agent.md"), filepath.Join(dir, "link.agent.md")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if _, err := DirSource(dir).Load(context.Background()); err == nil {
		t.Fatal("a link out of the directory was followed")
	}
}

func TestFSSource(t *testing.T) {
	fsys := fstest.MapFS{
		"defs/agents/a.md":       {Data: []byte(minimal("a", "1.0.0"))},
		"defs/b.agent.md":        {Data: []byte(minimal("b", "1.0.0"))},
		"defs/other/readme.md":   {Data: []byte("no")},
		"elsewhere/c.agent.md":   {Data: []byte(minimal("c", "1.0.0"))},
		"defs/huge/big.agent.md": {Data: []byte(minimal("big", "1.0.0") + strings.Repeat("x", MaxFileBytes))},
	}
	_, err := FSSource("embedded", fsys, "defs").Load(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("an oversized file was accepted: %v", err)
	}
	delete(fsys, "defs/huge/big.agent.md")
	defs, err := FSSource("embedded", fsys, "defs").Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(defs); !reflect.DeepEqual(got, []string{"a@1.0.0", "b@1.0.0"}) {
		t.Fatalf("got %v", got)
	}
	if defs[0].Location() != "embedded:defs/agents/a.md" && defs[1].Location() != "embedded:defs/agents/a.md" {
		t.Fatalf("locations %s %s", defs[0].Location(), defs[1].Location())
	}
}

func TestReaderSource(t *testing.T) {
	src := ReaderSource("blob", func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(minimal("r", "1.0.0"))), nil
	})
	defs, err := src.Load(context.Background())
	if err != nil || len(defs) != 1 || defs[0].Source != "blob" {
		t.Fatalf("got %v %v", defs, err)
	}
	failing := ReaderSource("blob", func(context.Context) (io.ReadCloser, error) { return nil, errors.New("gone") })
	if _, err := failing.Load(context.Background()); err == nil || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("got %v", err)
	}
}

func TestHTTPSourceETag(t *testing.T) {
	var hits, notModified atomic.Int32
	body := minimal("remote", "1.0.0")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "Bearer t" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("If-None-Match") == `"v1"` {
			notModified.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	src := HTTPSource(srv.URL+"/a.agent.md?token=secret", HTTPOptions{Client: srv.Client(), Header: http.Header{"Authorization": {"Bearer t"}}})
	if strings.Contains(sourceName(src), "secret") {
		t.Fatalf("the source name leaks the query: %s", sourceName(src))
	}
	for range 2 {
		defs, err := src.Load(context.Background())
		if err != nil || len(defs) != 1 || defs[0].Name != "remote" {
			t.Fatalf("got %v %v", defs, err)
		}
	}
	if hits.Load() != 2 || notModified.Load() != 1 {
		t.Fatalf("hits %d, not modified %d", hits.Load(), notModified.Load())
	}

	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
	defer plain.Close()
	if _, err := HTTPSource(plain.URL, HTTPOptions{}).Load(context.Background()); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("plain http was fetched: %v", err)
	}
	if _, err := HTTPSource(plain.URL, HTTPOptions{AllowInsecure: true}).Load(context.Background()); err != nil {
		t.Fatalf("AllowInsecure: %v", err)
	}
}

func TestLayeredOverrides(t *testing.T) {
	base, _ := Parse([]byte(minimal("a", "1.0.0")), "base")
	other, _ := Parse([]byte(minimal("a", "1.1.0")), "base")
	over, _ := Parse([]byte(minimal("a", "1.0.0")+"override"), "over")
	defs, err := Layered(StaticSource(base, other), StaticSource(over)).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(defs); !reflect.DeepEqual(got, []string{"a@1.0.0", "a@1.1.0"}) {
		t.Fatalf("got %v", got)
	}
	for _, d := range defs {
		if d.Version == "1.0.0" && d.Path != "over" {
			t.Fatalf("the later layer did not win: %s", d.Path)
		}
	}
	dup, _ := Parse([]byte(minimal("a", "1.0.0")+"x"), "dup")
	_, err = Layered(StaticSource(base, dup)).Load(context.Background())
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("duplicate in one layer: %v", err)
	}
}

func TestUntrustedLayer(t *testing.T) {
	ok := header("description: fine\nmodel: anthropic\ntools:\n  harness: [read, write]\n  registry: [rag_search]\n" +
		"skills: [go-style]\napproval:\n  ask: [write_file]\n  deny: [\"Bash(rm:*)\"]\n  deny_after: 2\n  grant: once\n" +
		"guardrails:\n  input: [pii]\nlimits:\n  max_iterations: 3\n  budget:\n    max_cost: 1\nmetadata:\n  a: b\n")
	d, err := Parse([]byte(ok), "project")
	if err != nil {
		t.Fatal(err)
	}
	defs, err := Untrusted(StaticSource(d)).Load(context.Background())
	if err != nil {
		t.Fatalf("allowed fields were refused: %v", err)
	}
	if defs[0].Trusted || !d.Trusted {
		t.Fatal("Untrusted must mark a copy, not the caller's definition")
	}
	refused := []struct{ lines, path string }{
		{"tools:\n  mcp: [github]\n", "tools.mcp"},
		{"tools:\n  harness: [exec]\n", "tools.harness[0]"},
		{"tools:\n  harness: [read, web]\n", "tools.harness[1]"},
		{"memory:\n  store: s\n", "memory"},
		{"approval:\n  allow: [read_file]\n", "approval.allow"},
		{"approval:\n  capabilities:\n    write: allow\n", "approval.capabilities"},
		{"approval:\n  ramp_after: 1\n", "approval.ramp_after"},
		{"dials:\n  creativity: creative\n", "dials"},
		{"compaction:\n  strategy: none\n", "compaction"},
		{"limits:\n  budget:\n    max_cost: 1\n    allow_unpriced: true\n", "limits.budget.allow_unpriced"},
	}
	for _, tc := range refused {
		d, err := Parse([]byte(header(tc.lines)), "project")
		if err != nil {
			t.Fatalf("%s: %v", tc.path, err)
		}
		_, err = Untrusted(StaticSource(d)).Load(context.Background())
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Issues[0].Code != CodeUntrusted || ve.Issues[0].Path != tc.path {
			t.Errorf("%s: got %v", tc.path, err)
		}
	}
}

// TestUntrustedFieldsComplete requires a decision for every field of every
// type a definition can hold, so a new field cannot be left undecided.
func TestUntrustedFieldsComplete(t *testing.T) {
	seen := map[reflect.Type]bool{}
	var walk func(reflect.Type)
	walk = func(t2 reflect.Type) {
		for t2.Kind() == reflect.Pointer || t2.Kind() == reflect.Slice || t2.Kind() == reflect.Map {
			t2 = t2.Elem()
		}
		if t2.Kind() != reflect.Struct || seen[t2] {
			return
		}
		seen[t2] = true
		table, ok := UntrustedFields[t2]
		if !ok {
			t.Errorf("UntrustedFields has no entry for %s", t2)
			return
		}
		if len(table) == 0 {
			return // the whole type is refused through its parent
		}
		for name := range fieldNames(t2) {
			if _, ok := table[name]; !ok {
				t.Errorf("UntrustedFields[%s] has no decision for %q", t2, name)
			}
		}
		for i := range t2.NumField() {
			if name, _, _ := strings.Cut(t2.Field(i).Tag.Get("json"), ","); name != "" && name != "-" && table[name] {
				walk(t2.Field(i).Type)
			}
		}
	}
	walk(reflect.TypeFor[Definition]())
}
