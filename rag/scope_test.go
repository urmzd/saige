package rag_test

import (
	"context"
	"errors"
	"testing"

	"github.com/urmzd/saige/rag"
	"github.com/urmzd/saige/rag/fusion"
	knowledgetypes "github.com/urmzd/saige/rag/knowledge/types"
	"github.com/urmzd/saige/rag/memstore"
	ragtypes "github.com/urmzd/saige/rag/types"
)

// groupGraph records the group of every ingested episode.
type groupGraph struct {
	factGraph
	groups []string
}

func (g *groupGraph) IngestEpisode(_ context.Context, in *knowledgetypes.EpisodeInput) (*knowledgetypes.IngestResult, error) {
	g.groups = append(g.groups, in.GroupID)
	return &knowledgetypes.IngestResult{}, nil
}

func TestWithScopeIsTheGraphNamespace(t *testing.T) {
	tests := []struct {
		name      string
		opts      []rag.Option
		wantGroup string
	}{
		{"scope becomes the namespace", []rag.Option{rag.WithScope("acme")}, "acme"},
		{"explicit namespace wins", []rag.Option{rag.WithScope("acme"), rag.WithGraphNamespace("shared")}, "shared"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &groupGraph{}
			opts := append([]rag.Option{
				rag.WithStore(memstore.New()),
				rag.WithContentExtractor(&stubExtractor{}),
				rag.WithGraph(g),
				rag.WithFuser(fusion.Weighted{Default: 1}),
			}, tt.opts...)
			pipe, err := rag.New(rag.Config{}, opts...)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pipe.Ingest(context.Background(), &ragtypes.RawDocument{SourceURI: "s://1", Data: []byte("text")}); err != nil {
				t.Fatal(err)
			}
			if len(g.groups) != 1 || g.groups[0] != tt.wantGroup {
				t.Errorf("episode groups = %v, want [%s]", g.groups, tt.wantGroup)
			}
		})
	}
}

// opaquePipeline is a Pipeline that cannot sync.
type opaquePipeline struct{ ragtypes.Pipeline }

type oneDocSource struct{}

func (oneDocSource) Fetch(context.Context) ([]ragtypes.RawDocument, error) {
	return []ragtypes.RawDocument{{SourceURI: "s://1", Data: []byte("synced")}}, nil
}

func TestSyncSourceHelper(t *testing.T) {
	ctx := context.Background()
	pipe, err := rag.New(rag.Config{}, rag.WithStore(memstore.New()), rag.WithContentExtractor(&stubExtractor{}))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		pipe    ragtypes.Pipeline
		wantErr error
	}{
		{"pipeline syncs", pipe, nil},
		{"pipeline without sync support", opaquePipeline{pipe}, ragtypes.ErrSyncUnsupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := rag.SyncSource(ctx, tt.pipe, oneDocSource{}, ragtypes.SyncOptions{})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && len(res.Created)+len(res.Unchanged) != 1 {
				t.Errorf("result = %+v", res)
			}
		})
	}
}
