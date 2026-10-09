package research

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestLongLinesAreReadAndSearched(t *testing.T) {
	root := t.TempDir()
	long := strings.Repeat("x", 200*1024)
	if err := os.WriteFile(filepath.Join(root, "min.js"), []byte(long+"\nneedle here\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		tool    types.Tool
		args    map[string]any
		want    []string
		wantNot []string
	}{
		{
			name: "read_file truncates the long line instead of failing",
			tool: NewReadFileTool(root),
			args: map[string]any{"path": "min.js"},
			want: []string{"more chars)", "   2 | needle here"},
		},
		{
			name: "read_file window after the long line",
			tool: NewReadFileTool(root),
			args: map[string]any{"path": "min.js", "offset": float64(2)},
			want: []string{"needle here"},
		},
		{
			name:    "file_search finds a match after a 100KB+ line",
			tool:    NewFileSearchTool(root),
			args:    map[string]any{"pattern": "needle"},
			want:    []string{"min.js:2: needle here"},
			wantNot: []string{"skipped"},
		},
		{
			name: "file_search truncates a long matching line",
			tool: NewFileSearchTool(root),
			args: map[string]any{"pattern": "^x+$"},
			want: []string{"min.js:1: ", "more chars)"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.tool.Execute(context.Background(), tt.args)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if len(got) > 10*1024 {
				t.Errorf("output is %d bytes; long lines must be truncated", len(got))
			}
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("output missing %q", w)
				}
			}
			for _, w := range tt.wantNot {
				if strings.Contains(got, w) {
					t.Errorf("output unexpectedly contains %q", w)
				}
			}
		})
	}
}

func TestTruncateLineKeepsRunesWhole(t *testing.T) {
	tests := []struct {
		in   string
		max  int
		want string
	}{
		{"short", 10, "short"},
		{"abcdef", 3, "abc...(3 more chars)"},
		{"héllo", 2, "h...(5 more chars)"},
	}
	for _, tt := range tests {
		if got := truncateLine(tt.in, tt.max); got != tt.want {
			t.Errorf("truncateLine(%q, %d) = %q, want %q", tt.in, tt.max, got, tt.want)
		}
	}
}

func TestResearchToolsDeclareCapabilities(t *testing.T) {
	root := t.TempDir()
	for _, tool := range []types.Tool{NewReadFileTool(root), NewFileSearchTool(root), NewWebSearchTool(nil, nil)} {
		if got := tool.Definition().Capability; got != types.ToolCapabilityRead {
			t.Errorf("%s capability = %q, want read", tool.Definition().Name, got)
		}
	}
	if got := NewStoreKnowledgeTool(nil).Definition().Capability; got != types.ToolCapabilityWrite {
		t.Errorf("store_knowledge capability = %q, want write", got)
	}
}
