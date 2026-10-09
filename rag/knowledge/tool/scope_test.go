package tool_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	agenttypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/rag/knowledge/tool"
	kgtypes "github.com/urmzd/saige/rag/knowledge/types"
)

// recordingGraph records the options and inputs the tools pass.
type recordingGraph struct {
	stubGraph
	search    kgtypes.SearchOptions
	ingest    *kgtypes.EpisodeInput
	ingestErr error
}

func (g *recordingGraph) SearchFacts(_ context.Context, _ string, opts ...kgtypes.SearchOption) (*kgtypes.SearchFactsResult, error) {
	for _, o := range opts {
		o(&g.search)
	}
	return &kgtypes.SearchFactsResult{}, nil
}

func (g *recordingGraph) IngestEpisode(_ context.Context, in *kgtypes.EpisodeInput) (*kgtypes.IngestResult, error) {
	g.ingest = in
	return &kgtypes.IngestResult{UUID: "ep-1"}, g.ingestErr
}

func toolNamed(t *testing.T, tools []agenttypes.Tool, name string) agenttypes.Tool {
	t.Helper()
	for _, tl := range tools {
		if tl.Definition().Name == name {
			return tl
		}
	}
	t.Fatalf("%s not found", name)
	return nil
}

func TestToolsGroupScope(t *testing.T) {
	tests := []struct {
		name      string
		opts      []tool.Option
		wantGroup string
	}{
		{"bound group", []tool.Option{tool.WithGroupID("t1")}, "t1"},
		{"no group", nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := &recordingGraph{}
			tools := tool.NewTools(g, tc.opts...)

			// A model-supplied group_id argument is ignored.
			if _, err := toolNamed(t, tools, "kg_search").Execute(context.Background(),
				map[string]any{"query": "q", "group_id": "other"}); err != nil {
				t.Fatal(err)
			}
			if g.search.GroupID != tc.wantGroup {
				t.Errorf("search GroupID = %q, want %q", g.search.GroupID, tc.wantGroup)
			}

			if _, err := toolNamed(t, tools, "kg_ingest").Execute(context.Background(),
				map[string]any{"name": "n", "body": "b", "group_id": "other"}); err != nil {
				t.Fatal(err)
			}
			if g.ingest == nil || g.ingest.GroupID != tc.wantGroup {
				t.Errorf("ingest input = %+v, want GroupID %q", g.ingest, tc.wantGroup)
			}
		})
	}
}

func TestSearchToolClampsLimit(t *testing.T) {
	tests := []struct {
		name  string
		limit any
		want  int
	}{
		{"huge", float64(10000), tool.MaxSearchLimit},
		{"small", float64(5), 5},
		{"json number", json.Number("7"), 7},
		{"missing", nil, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := &recordingGraph{}
			args := map[string]any{"query": "q"}
			if tc.limit != nil {
				args["limit"] = tc.limit
			}
			if _, err := toolNamed(t, tool.NewTools(g), "kg_search").Execute(context.Background(), args); err != nil {
				t.Fatal(err)
			}
			if g.search.Limit != tc.want {
				t.Errorf("limit = %d, want %d", g.search.Limit, tc.want)
			}
		})
	}
}

func TestIngestToolPartialAndHardErrors(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantErr     bool
		wantWarning bool
	}{
		{"partial", fmt.Errorf("%w: entity C failed", kgtypes.ErrPartialEpisode), false, true},
		{"hard", errors.New("db down"), true, false},
		{"ok", nil, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := &recordingGraph{ingestErr: tc.err}
			out, err := toolNamed(t, tool.NewTools(g), "kg_ingest").Execute(context.Background(),
				map[string]any{"name": "n", "body": "b"})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if !strings.Contains(out, `"uuid":"ep-1"`) {
				t.Errorf("output %s missing the episode", out)
			}
			if got := strings.Contains(out, `"warning"`); got != tc.wantWarning {
				t.Errorf("output %s: warning present = %v, want %v", out, got, tc.wantWarning)
			}
		})
	}
}
