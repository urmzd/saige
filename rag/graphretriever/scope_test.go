package graphretriever_test

import (
	"context"
	"testing"
	"time"

	"github.com/urmzd/saige/rag/graphretriever"
	knowledgetypes "github.com/urmzd/saige/rag/knowledge/types"
	"github.com/urmzd/saige/rag/memstore"
	ragtypes "github.com/urmzd/saige/rag/types"
)

// plainStore hides the optional record lookups of the store it wraps, so the
// retriever must build records from GetVariant and GetDocument.
type plainStore struct{ ragtypes.Store }

func TestGraphRetrieverScope(t *testing.T) {
	for _, plain := range []bool{false, true} {
		name := "record store"
		if plain {
			name = "plain store"
		}
		t.Run(name, func(t *testing.T) { testScope(t, plain) })
	}
}

func testScope(t *testing.T, plain bool) {
	ctx := context.Background()
	mem := memstore.New()
	var store ragtypes.Store = mem
	if plain {
		store = plainStore{mem}
	}
	ts := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	for _, d := range []struct{ uuid, scope string }{{"da", "a"}, {"db", "b"}} {
		doc := &ragtypes.Document{UUID: d.uuid, Scope: d.scope, SourceModifiedAt: ts, Sections: []ragtypes.Section{{
			UUID: d.uuid + "-s", DocumentUUID: d.uuid,
			Variants: []ragtypes.ContentVariant{{UUID: d.uuid + "-v", ContentType: ragtypes.ContentText, Text: "fact source"}},
		}}}
		if err := store.CreateDocument(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	graph := &mockGraph{
		facts: []knowledgetypes.Fact{
			{UUID: "fa", FactText: "a"}, {UUID: "fb", FactText: "b"}, {UUID: "orphan", FactText: "o"},
			{UUID: "gone-variant", FactText: "gv"}, {UUID: "gone-doc", FactText: "gd"},
		},
		episodes: map[string][]knowledgetypes.Episode{
			"fa":           {{Metadata: map[string]string{"variant_uuid": "da-v"}}},
			"fb":           {{Metadata: map[string]string{"variant_uuid": "db-v"}}},
			"gone-variant": {{Metadata: map[string]string{"variant_uuid": "deleted-v"}}},
			"gone-doc":     {{Name: "Intro", DocumentID: "deleted-doc"}},
		},
	}

	tests := []struct {
		name  string
		group string
		opts  *ragtypes.SearchOptions
		want  []string
	}{
		{"scope a keeps its document and its group's facts", "a", &ragtypes.SearchOptions{Scope: "a"}, []string{"da-v", "orphan"}},
		{"synthetic facts dropped outside the graph group", "", &ragtypes.SearchOptions{Scope: "b"}, []string{"db-v"}},
		{"time range drops undated synthetic facts", "a", &ragtypes.SearchOptions{Scope: "a", Since: ts}, []string{"da-v"}},
		{"time range excludes older documents", "a", &ragtypes.SearchOptions{Scope: "a", Since: ts.Add(time.Hour)}, nil},
		{"default scope sees no scoped document or attributed fact", "", &ragtypes.SearchOptions{}, []string{"orphan"}},
		{"nil options match the default scope", "", nil, []string{"orphan"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := graphretriever.New(graph, store, graphretriever.WithGroupID(tt.group))
			hits, err := r.Retrieve(ctx, "q", tt.opts)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, h := range hits {
				got = append(got, h.Variant.UUID)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("hits = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("hits = %v, want %v", got, tt.want)
				}
			}
		})
	}
}
