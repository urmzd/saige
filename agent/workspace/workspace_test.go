package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/urmzd/saige/agent/privacy"
	"github.com/urmzd/saige/agent/types"
)

func backends(t *testing.T) map[string]Workspace {
	t.Helper()
	dir, err := NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return map[string]Workspace{"memory": NewMemory(), "dir": dir}
}

func TestWorkspaceContract(t *testing.T) {
	ctx := context.Background()
	for name, ws := range backends(t) {
		t.Run(name, func(t *testing.T) {
			meta := map[string]string{"k": "v"}
			ref, err := ws.Put(ctx, "notes/a.md", []byte("alpha line\nBeta Gamma line\n"), meta)
			if err != nil {
				t.Fatal(err)
			}
			meta["k"] = "changed"
			if ref.ID != Digest([]byte("alpha line\nBeta Gamma line\n")) || ref.Size != 27 || ref.Meta["k"] != "v" {
				t.Fatalf("ref = %+v", ref)
			}
			again, err := ws.Put(ctx, "notes/a.md", []byte("alpha line\nBeta Gamma line\n"), nil)
			if err != nil || again.ID != ref.ID {
				t.Fatalf("repeat put = %+v, %v", again, err)
			}

			for _, tc := range []struct {
				name          string
				ref           Ref
				offset, limit int64
				want          string
				err           error
			}{
				{"by id", Ref{ID: ref.ID}, 0, 0, "alpha line\nBeta Gamma line\n", nil},
				{"by name", Ref{Name: "notes/a.md"}, 0, 5, "alpha", nil},
				{"window", Ref{ID: ref.ID}, 6, 4, "line", nil},
				{"past end", Ref{ID: ref.ID}, 100, 0, "", nil},
				{"missing name", Ref{Name: "nope"}, 0, 0, "", ErrNotFound},
				{"missing id", Ref{ID: Digest([]byte("x"))}, 0, 0, "", ErrNotFound},
				{"empty", Ref{}, 0, 0, "", ErrInvalidRef},
			} {
				t.Run(tc.name, func(t *testing.T) {
					got, err := ws.Read(ctx, tc.ref, tc.offset, tc.limit)
					if tc.err != nil {
						if !errors.Is(err, tc.err) {
							t.Fatalf("err = %v, want %v", err, tc.err)
						}
						return
					}
					if err != nil || string(got) != tc.want {
						t.Fatalf("Read = %q, %v", got, err)
					}
				})
			}

			if _, err := ws.Put(ctx, "notes/a.md", []byte("replaced"), nil); err != nil {
				t.Fatal(err)
			}
			if got, _ := ws.Read(ctx, Ref{Name: "notes/a.md"}, 0, 0); string(got) != "replaced" {
				t.Fatalf("name not updated: %q", got)
			}
			// The old content is still addressable by its digest.
			if got, _ := ws.Read(ctx, Ref{ID: ref.ID}, 0, 0); !strings.HasPrefix(string(got), "alpha") {
				t.Fatalf("old content lost: %q", got)
			}

			if _, err := ws.Put(ctx, "b.txt", []byte("one\ntwo gamma beta\n"), nil); err != nil {
				t.Fatal(err)
			}
			hits, err := ws.Search(ctx, "BETA gamma", 10)
			if err != nil || len(hits) != 1 || hits[0].Ref.Name != "b.txt" || hits[0].Line != 2 || hits[0].Offset != 4 {
				t.Fatalf("Search = %+v, %v", hits, err)
			}
			if _, err := ws.Search(ctx, "  ", 1); err == nil {
				t.Fatal("empty query must fail")
			}
			list, err := ws.List(ctx)
			if err != nil || len(list) != 2 || list[0].Name != "b.txt" || list[1].Name != "notes/a.md" {
				t.Fatalf("List = %+v, %v", list, err)
			}
			if _, err := ws.Put(ctx, " ", nil, nil); !errors.Is(err, ErrInvalidRef) {
				t.Fatalf("blank name err = %v", err)
			}

			ro := ws.View(true)
			if _, err := ro.Put(ctx, "c", []byte("c"), nil); !errors.Is(err, ErrReadOnly) {
				t.Fatalf("read-only put err = %v", err)
			}
			if _, err := ro.View(false).Put(ctx, "c", []byte("c"), nil); !errors.Is(err, ErrReadOnly) {
				t.Fatal("a view of a read-only view became writable")
			}
			if got, _ := ro.Read(ctx, Ref{Name: "b.txt"}, 0, 3); string(got) != "one" {
				t.Fatalf("read-only read = %q", got)
			}
		})
	}
}

func TestParseRef(t *testing.T) {
	id := Digest([]byte("x"))
	for _, tc := range []struct {
		in   string
		want Ref
		err  bool
	}{
		{URIScheme + "://" + id, Ref{ID: id}, false},
		{id, Ref{ID: id}, false},
		{"notes/a.md", Ref{Name: "notes/a.md"}, false},
		{URIScheme + "://short", Ref{}, true},
		{"", Ref{}, true},
	} {
		got, err := ParseRef(tc.in)
		if (err != nil) != tc.err || got.ID != tc.want.ID || got.Name != tc.want.Name {
			t.Fatalf("ParseRef(%q) = %+v, %v", tc.in, got, err)
		}
	}
	if (Ref{ID: id}).URI() != URIScheme+"://"+id {
		t.Fatal("URI")
	}
}

func TestDirAtomicLayout(t *testing.T) {
	root := t.TempDir()
	d, err := NewDir(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := d.Put(ctx, "shared", []byte("same bytes"), nil); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	for _, sub := range []string{"objects", "names"} {
		entries, _ := os.ReadDir(filepath.Join(root, sub))
		if len(entries) != 1 || strings.HasPrefix(entries[0].Name(), ".tmp-") {
			t.Fatalf("%s holds %v, want one final file", sub, entries)
		}
	}
	// A reopened workspace sees the same artifacts.
	d2, _ := NewDir(root)
	if got, err := d2.Read(ctx, Ref{Name: "shared"}, 0, 0); err != nil || string(got) != "same bytes" {
		t.Fatalf("reopen read = %q, %v", got, err)
	}
}

func run(t *testing.T, tool types.Tool, ctx context.Context, args map[string]any) (string, error) {
	t.Helper()
	return tool.Execute(ctx, args)
}

func TestTools(t *testing.T) {
	ctx := context.Background()
	ws := NewMemory()
	tools := Tools(ws)
	if len(tools) != 3 {
		t.Fatal(len(tools))
	}
	caps := map[string]types.ToolCapability{}
	for _, tl := range tools {
		caps[tl.Definition().Name] = tl.Definition().Capability
	}
	if caps[WriteToolName] != types.ToolCapabilityWrite || caps[ReadToolName] != types.ToolCapabilityRead || caps[SearchToolName] != types.ToolCapabilityRead {
		t.Fatalf("capabilities = %v", caps)
	}

	out, err := run(t, tools[0], ctx, map[string]any{"name": "plan", "content": "step one\nstep two é"})
	if err != nil || !strings.Contains(out, URIScheme+"://") {
		t.Fatalf("write = %q, %v", out, err)
	}
	uri := out[strings.Index(out, URIScheme):]

	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"by name", map[string]any{"ref": "plan"}, "step one\nstep two é"},
		{"by uri paged", map[string]any{"ref": uri, "limit": 4.0}, "step\n[4 of 20 bytes shown; continue with offset 4]"},
		{"page cuts before a split character", map[string]any{"ref": "plan", "offset": 9.0, "limit": 10.0}, "step two \n[9 of 20 bytes shown; continue with offset 18]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := run(t, tools[1], ctx, tc.args)
			if err != nil || got != tc.want {
				t.Fatalf("read = %q, %v; want %q", got, err, tc.want)
			}
		})
	}

	got, err := run(t, tools[2], ctx, map[string]any{"query": "two"})
	if err != nil || !strings.HasPrefix(got, "plan:2 ") {
		t.Fatalf("search = %q, %v", got, err)
	}

	// A read-only view attached to the context wins over the store the
	// tool was built with.
	roCtx := NewContext(ctx, ws.View(true))
	if _, err := run(t, tools[0], roCtx, map[string]any{"name": "x", "content": "y"}); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("write through read-only view err = %v", err)
	}
	if _, err := run(t, WriteTool(nil), ctx, map[string]any{"name": "x", "content": "y"}); err == nil {
		t.Fatal("tool without a workspace must fail")
	}
}

type richEcho struct{ text string }

func (r richEcho) Definition() types.ToolDef { return types.ToolDef{Name: "big"} }
func (r richEcho) Execute(context.Context, map[string]any) (string, error) {
	return r.text, nil
}
func (r richEcho) ExecuteRich(context.Context, map[string]any) (types.ToolResult, error) {
	return types.ToolResult{Parts: []types.ToolOutputPart{
		types.Text(r.text),
		types.Image(types.Bytes(types.MediaPNG, []byte("img"))),
	}}, nil
}

func TestSpill(t *testing.T) {
	ctx := context.Background()
	big := strings.Repeat("0123456789", 10)
	opts := SpillOptions{MaxBytes: 50, PreviewBytes: 20}
	for _, tc := range []struct {
		name    string
		tool    types.Tool
		ws      Workspace
		spilled bool
	}{
		{"small result passes", richEcho{"short"}, NewMemory(), false},
		{"large plain result spills", &types.ToolFunc{Def: types.ToolDef{Name: "big"}, Fn: func(context.Context, map[string]any) (string, error) { return big, nil }}, NewMemory(), true},
		{"large rich result spills", richEcho{big}, NewMemory(), true},
		{"read-only store keeps the whole result", richEcho{big}, NewMemory().View(true), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := Spill(tc.tool, tc.ws, opts).(types.RichTool)
			res, err := wrapped.ExecuteRich(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.spilled {
				raw, _ := tc.tool.Execute(ctx, nil)
				if res.Text() != raw {
					t.Fatalf("text changed: %q", res.Text())
				}
				return
			}
			if !strings.HasPrefix(res.Text(), big[:20]+"\n\n[result truncated: showing 20 of 100 bytes") {
				t.Fatalf("preview = %q", res.Text())
			}
			i := strings.Index(res.Text(), URIScheme)
			uri := strings.TrimSuffix(strings.Fields(res.Text()[i:])[0], ".")
			ref, _ := ParseRef(uri)
			full, err := tc.ws.Read(ctx, ref, 0, 0)
			if err != nil || string(full) != big {
				t.Fatalf("stored = %q, %v", full, err)
			}
			if len(res.Parts) > 1 && (res.Parts[0].(types.TextPart).Text != res.Text() || string(res.Parts[1].(types.ImagePart).Source.Inline) != "img") {
				t.Fatalf("parts = %+v", res.Parts)
			}
		})
	}

	marked := types.WithMarkers(richEcho{big}, types.Marker{Kind: "human_approval"})
	out, ok := Spill(marked, NewMemory(), opts).(*types.MarkedTool)
	if !ok || len(out.Markers) != 1 {
		t.Fatal("markers must stay outermost")
	}
}

func TestSpillPreviewKeepsValuesWhole(t *testing.T) {
	ctx := context.Background()
	redactor := privacy.NewToolRedactor(privacy.NewVault(privacy.DefaultDetector()))
	for _, tc := range []struct {
		name string
		text string
	}{
		{"email crosses the cut", "contact ada.lovelace@example.com about the report " + strings.Repeat("x", 80)},
		{"card crosses the cut", "card on file 4111 1111 1111 1111 expires soon " + strings.Repeat("x", 80)},
		{"json without spaces", `{"id":1,"email":"ada.lovelace@example.com","n":` + strings.Repeat("9", 80) + `}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := &types.ToolFunc{Def: types.ToolDef{Name: "big"}, Fn: func(context.Context, map[string]any) (string, error) { return tc.text, nil }}
			wrapped := Spill(tool, NewMemory(), SpillOptions{MaxBytes: 50, PreviewBytes: 28})
			out, err := wrapped.Execute(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			res := redactor.TokenizeResult(ctx, tool.Definition(), types.ToolResult{Parts: []types.ToolOutputPart{types.Text(out)}})
			preview := res.Text()[:strings.Index(res.Text(), "\n\n[result truncated")]
			for _, frag := range []string{"ada", "lovelace", "example", "4111", "1111"} {
				if strings.Contains(preview, frag) {
					t.Fatalf("preview leaks %q: %q", frag, preview)
				}
			}
		})
	}
}

func TestCutAtBoundary(t *testing.T) {
	for _, tc := range []struct {
		name  string
		s     string
		limit int
		want  string
	}{
		{"whole text fits", "a b", 10, "a b"},
		{"cuts after a space", "hello world again", 13, "hello world "},
		{"newline wins inside a number", "line\n4111 1111 1111 1111", 12, "line\n"},
		{"skips spaces inside grouped digits", "pay 4111 1111 1111 1111", 16, "pay "},
		{"skips space after a phone area code", "call (555) 123-4567", 14, "call "},
		{"cuts after a json quote", `{"a":1,"email":"x@y.io"}`, 18, `{"a":1,"email":"`},
		{"unbroken token falls back to characters", "abcdéf", 5, "abcd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s[:cutAtBoundary(tc.s, tc.limit)]; got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
