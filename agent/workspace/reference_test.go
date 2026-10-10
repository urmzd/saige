package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestLayersReadThroughWriteTop(t *testing.T) {
	ctx := context.Background()
	base, top := NewMemory(), NewMemory()
	if _, err := base.Put(ctx, "shared", []byte("from parent"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := base.Put(ctx, "notes", []byte("parent notes"), nil); err != nil {
		t.Fatal(err)
	}
	l := NewLayers(top, base.View(true))
	if _, err := l.Put(ctx, "notes", []byte("child notes"), nil); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"shared": "from parent", "notes": "child notes"} {
		got, err := l.Read(ctx, Ref{Name: name}, 0, 0)
		if err != nil || string(got) != want {
			t.Fatalf("Read(%s) = %q, %v, want %q", name, got, err, want)
		}
	}
	if got, _ := base.Read(ctx, Ref{Name: "notes"}, 0, 0); string(got) != "parent notes" {
		t.Fatalf("write reached the base layer: %q", got)
	}
	refs, err := l.List(ctx)
	if err != nil || len(refs) != 2 {
		t.Fatalf("List = %+v, %v", refs, err)
	}
	hits, err := l.Search(ctx, "notes", 10)
	if err != nil || len(hits) != 2 || !strings.Contains(hits[0].Text, "child") {
		t.Fatalf("Search = %+v, %v", hits, err)
	}
	if _, err := l.Read(ctx, Ref{Name: "missing"}, 0, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing err = %v", err)
	}
	if _, err := l.View(true).Put(ctx, "x", nil, nil); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("read-only view err = %v", err)
	}
	if _, err := NewLayers(nil, base).Put(ctx, "x", nil, nil); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("no top err = %v", err)
	}

	late := NewMemory()
	ref, _ := late.Put(ctx, "late", []byte("attached later"), nil)
	l.Attach(late)
	if got, err := l.Read(ctx, Ref{ID: ref.ID}, 0, 0); err != nil || string(got) != "attached later" {
		t.Fatalf("attached read = %q, %v", got, err)
	}
}

func TestNewReferencePreview(t *testing.T) {
	ctx := context.Background()
	ws := NewMemory()
	data := strings.Repeat("word ", 1000)
	r, err := NewReference(ctx, ws, "big", []byte(data), ReferenceOptions{PreviewTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	if r.Tokens != EstimateTokens(data) || len(r.Preview) > 40 || !strings.HasPrefix(data, r.Preview) {
		t.Fatalf("reference = %+v", r)
	}
	s := r.String()
	for _, want := range []string{r.URI(), ReadArtifactToolName, SearchArtifactToolName, "Preview:"} {
		if !strings.Contains(s, want) {
			t.Fatalf("String() lacks %q:\n%s", want, s)
		}
	}
	if len(s) > 600 {
		t.Fatalf("String() is %d bytes, want a short reference", len(s))
	}
	if r2, _ := NewReference(ctx, ws, "big", []byte(data), ReferenceOptions{}); r2.URI() != r.URI() {
		t.Fatal("same content got a new URI")
	}
	if r3, _ := NewReference(ctx, ws, "small", []byte("hi"), ReferenceOptions{PreviewTokens: -1}); r3.Preview != "" {
		t.Fatalf("negative preview = %q", r3.Preview)
	}
	if _, err := NewReference(ctx, nil, "x", nil, ReferenceOptions{}); err == nil {
		t.Fatal("nil workspace accepted")
	}
}

func TestCopyAndCopyTools(t *testing.T) {
	ctx := context.Background()
	ws := NewMemory()
	src, _ := ws.Put(ctx, "a", []byte("payload"), map[string]string{"k": "v"})
	dst := NewMemory()
	ref, err := Copy(ctx, ws, Ref{Name: "a"}, dst, "b")
	if err != nil || ref.ID != src.ID || ref.Meta["copied_from"] != "a" || ref.Meta["k"] != "v" {
		t.Fatalf("Copy = %+v, %v", ref, err)
	}

	out, err := CopyArtifactTool(ws).Execute(ctx, map[string]any{"from": src.URI(), "name": "c"})
	if err != nil || strings.Contains(out, "payload") || !strings.Contains(out, src.URI()) {
		t.Fatalf("copy_artifact = %q, %v", out, err)
	}

	root := t.TempDir()
	body := strings.Repeat("line of the file\n", 500)
	if err := os.WriteFile(filepath.Join(root, "doc.txt"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := CopyFileTool(ws, root)
	out, err = tool.Execute(ctx, map[string]any{"path": "doc.txt"})
	if err != nil || !strings.Contains(out, URIScheme+"://"+Digest([]byte(body))) || len(out) >= len(body)/4 {
		t.Fatalf("copy_file = %d bytes, %v", len(out), err)
	}
	if got, _ := ws.Read(ctx, Ref{Name: "files/doc.txt"}, 0, 0); string(got) != body {
		t.Fatal("copy_file stored different content")
	}
	if _, err := tool.Execute(ctx, map[string]any{"path": "../outside.txt"}); err == nil {
		t.Fatal("copy_file escaped its root")
	}
}

func TestSearchArtifactInOneArtifact(t *testing.T) {
	ctx := context.Background()
	ws := NewMemory()
	a, _ := ws.Put(ctx, "a", []byte("alpha\nneedle in a\n"), nil)
	if _, err := ws.Put(ctx, "b", []byte("needle in b\n"), nil); err != nil {
		t.Fatal(err)
	}
	tool := SearchArtifactTool(ws)
	out, err := tool.Execute(ctx, map[string]any{"query": "needle", "uri": a.URI()})
	if err != nil || !strings.Contains(out, "needle in a") || strings.Contains(out, "needle in b") {
		t.Fatalf("one artifact = %q, %v", out, err)
	}
	out, err = tool.Execute(ctx, map[string]any{"query": "needle"})
	if err != nil || !strings.Contains(out, "needle in b") {
		t.Fatalf("all artifacts = %q, %v", out, err)
	}
	read, err := ReadArtifactTool(ws).Execute(ctx, map[string]any{"uri": a.URI(), "offset": 6})
	if err != nil || read != "needle in a\n" {
		t.Fatalf("read_artifact = %q, %v", read, err)
	}
}

func TestSpillUsesAttachedWorkspace(t *testing.T) {
	ctx := context.Background()
	big := strings.Repeat("x ", 100)
	tool := Spill(fixedTool(big), nil, SpillOptions{MaxBytes: 20})
	if out, _ := tool.Execute(ctx, nil); out != big {
		t.Fatal("spilled with no workspace anywhere")
	}
	ws := NewMemory()
	out, _ := tool.Execute(NewContext(ctx, ws), nil)
	if !strings.Contains(out, "result truncated") {
		t.Fatalf("not spilled into the attached workspace: %q", out)
	}
}

func fixedTool(text string) types.Tool {
	return &types.ToolFunc{Def: types.ToolDef{Name: "fixed"}, Fn: func(context.Context, map[string]any) (string, error) { return text, nil }}
}
