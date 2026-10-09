package memstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/urmzd/saige/rag/memstore"
	"github.com/urmzd/saige/rag/types"
)

func TestMemstoreScopeTimeAndSources(t *testing.T) {
	ctx := context.Background()
	s := memstore.New()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	add := func(uuid, scope, uri string, updated, modified time.Time) {
		doc := &types.Document{UUID: uuid, Scope: scope, SourceURI: uri, UpdatedAt: updated, SourceModifiedAt: modified,
			Fingerprint: types.Fingerprint(scope, []byte(uuid)),
			Sections: []types.Section{{UUID: uuid + "-s", DocumentUUID: uuid, Variants: []types.ContentVariant{{
				UUID: uuid + "-v", ContentType: types.ContentText, Embedding: []float32{1, 0},
			}}}}}
		if err := s.CreateDocument(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	add("a1", "a", "fs://x/1", t0, time.Time{})
	add("a2", "a", "fs://x/1", t0.Add(time.Hour), t0.Add(-time.Hour))
	add("a3", "a", "fs://y/1", t0, t0.AddDate(0, 2, 0))
	add("b1", "b", "fs://x/1", t0, time.Time{})

	searchTests := []struct {
		name string
		opts *types.SearchOptions
		want map[string]bool
	}{
		{"scope a", &types.SearchOptions{Scope: "a"}, map[string]bool{"a1-v": true, "a2-v": true, "a3-v": true}},
		{"scope b", &types.SearchOptions{Scope: "b"}, map[string]bool{"b1-v": true}},
		{"nil options are the default scope", nil, map[string]bool{}},
		{"since uses the source time first", &types.SearchOptions{Scope: "a", Since: t0}, map[string]bool{"a1-v": true, "a3-v": true}},
		{"until", &types.SearchOptions{Scope: "a", Until: t0}, map[string]bool{"a2-v": true}},
	}
	for _, tt := range searchTests {
		t.Run(tt.name, func(t *testing.T) {
			hits, err := s.SearchByEmbedding(ctx, []float32{1, 0}, tt.opts)
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
			}
		})
	}

	found, err := s.FindBySourceURI(ctx, "a", "fs://x/1")
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 || found[0].UUID != "a2" || found[1].UUID != "a1" {
		t.Errorf("FindBySourceURI = %+v, want a2 then a1", found)
	}
	if none, _ := s.FindBySourceURI(ctx, "c", "fs://x/1"); none == nil || len(none) != 0 {
		t.Errorf("missing URI must give an empty slice, got %#v", none)
	}
	listed, err := s.ListSourceDocuments(ctx, "a", "fs://x/")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0].SourceURI != "fs://x/1" || !listed[0].SourceModifiedAt.Equal(t0.Add(-time.Hour)) {
		t.Errorf("ListSourceDocuments = %+v", listed)
	}
	rec, err := s.GetVariantRecord(ctx, "a3-v")
	if err != nil || rec.Scope != "a" || !rec.Timestamp.Equal(t0.AddDate(0, 2, 0)) {
		t.Errorf("record = %+v, %v", rec, err)
	}
}
