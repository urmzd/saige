package bm25retriever_test

import (
	"context"
	"testing"
	"time"

	"github.com/urmzd/saige/rag/bm25retriever"
	"github.com/urmzd/saige/rag/memstore"
	"github.com/urmzd/saige/rag/types"
)

func TestBM25ScopeAndTimeRange(t *testing.T) {
	for _, plain := range []bool{false, true} {
		name := "record store"
		if plain {
			name = "plain store"
		}
		t.Run(name, func(t *testing.T) { testScopeAndTimeRange(t, plain) })
	}
}

func testScopeAndTimeRange(t *testing.T, plain bool) {
	ctx := context.Background()
	mem := memstore.New()
	var store types.Store = mem
	if plain {
		store = plainStore{mem}
	}
	r := bm25retriever.New(store, nil)
	jan := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	docs := []struct {
		uuid, scope string
		modified    time.Time
	}{
		{"a-old", "a", jan},
		{"a-new", "a", jan.AddDate(0, 3, 0)},
		{"b-new", "b", jan.AddDate(0, 3, 0)},
	}
	for _, d := range docs {
		doc := &types.Document{UUID: d.uuid, Scope: d.scope, SourceModifiedAt: d.modified, Sections: []types.Section{{
			UUID: d.uuid + "-s", DocumentUUID: d.uuid,
			Variants: []types.ContentVariant{{UUID: d.uuid + "-v", ContentType: types.ContentText, Text: "budget forecast"}},
		}}}
		if err := store.CreateDocument(ctx, doc); err != nil {
			t.Fatal(err)
		}
		if err := r.Index(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name string
		opts *types.SearchOptions
		want map[string]bool
	}{
		{"scope a", &types.SearchOptions{Scope: "a"}, map[string]bool{"a-old-v": true, "a-new-v": true}},
		{"scope b", &types.SearchOptions{Scope: "b"}, map[string]bool{"b-new-v": true}},
		{"default scope sees none", &types.SearchOptions{}, map[string]bool{}},
		{"nil options see the default scope only", nil, map[string]bool{}},
		{"scope a since february", &types.SearchOptions{Scope: "a", Since: jan.AddDate(0, 1, 0)}, map[string]bool{"a-new-v": true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hits, err := r.Retrieve(ctx, "budget", tt.opts)
			if err != nil {
				t.Fatal(err)
			}
			if len(hits) != len(tt.want) {
				t.Fatalf("got %d hits, want %v", len(hits), tt.want)
			}
			for _, h := range hits {
				if !tt.want[h.Variant.UUID] {
					t.Errorf("unexpected hit %s", h.Variant.UUID)
				}
				if tt.opts == nil || tt.opts.Since.IsZero() {
					continue
				}
				if h.Timestamp.Before(tt.opts.Since) {
					t.Errorf("hit %s timestamp %v before %v", h.Variant.UUID, h.Timestamp, tt.opts.Since)
				}
			}
		})
	}
}
