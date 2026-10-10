package pgstore

import (
	"context"
	"testing"
	"time"

	"github.com/urmzd/saige/internal/must"
	"github.com/urmzd/saige/rag/types"
)

// TestStoreScopeTimeAndSources covers scope isolation, the effective time
// range, and the source-URI lookups against PostgreSQL.
func TestStoreScopeTimeAndSources(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	s := must.Get(New(Config{Pool: pool}))

	t0 := testTime().Add(-48 * time.Hour)
	add := func(uuid, scope, uri string, updated, modified time.Time, v []float32) {
		t.Helper()
		doc := singleSectionDoc(uuid, types.Fingerprint(scope, []byte(uri)), nil,
			types.ContentVariant{UUID: uuid + "-v", ContentType: types.ContentText, Text: "t", Embedding: v})
		doc.Scope = scope
		doc.SourceURI = uri
		doc.CreatedAt, doc.UpdatedAt = updated, updated
		doc.SourceModifiedAt = modified
		if err := s.CreateDocument(ctx, doc); err != nil {
			t.Fatalf("create %s: %v", uuid, err)
		}
	}
	// Identical bytes (here the URI) in two scopes do not trip the unique
	// fingerprint index.
	add("a1", "a", "fs://x/1", t0, time.Time{}, vec(1, 0))
	add("b1", "b", "fs://x/1", t0, time.Time{}, vec(1, 0))
	add("a2", "a", "fs://x/2", t0.Add(time.Hour), t0.Add(-24*time.Hour), vec(0.9, 0.1))

	doc, err := s.GetDocument(ctx, "a2")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Scope != "a" || !doc.SourceModifiedAt.Equal(t0.Add(-24*time.Hour)) {
		t.Errorf("GetDocument scope=%q modified=%v", doc.Scope, doc.SourceModifiedAt)
	}

	tests := []struct {
		name string
		opts *types.SearchOptions
		want map[string]bool
	}{
		{"scope a", &types.SearchOptions{Scope: "a"}, map[string]bool{"a1-v": true, "a2-v": true}},
		{"scope b", &types.SearchOptions{Scope: "b"}, map[string]bool{"b1-v": true}},
		{"default scope", &types.SearchOptions{}, map[string]bool{}},
		{"since uses the source time", &types.SearchOptions{Scope: "a", Since: t0.Add(-time.Hour)}, map[string]bool{"a1-v": true}},
		{"until", &types.SearchOptions{Scope: "a", Until: t0}, map[string]bool{"a2-v": true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hits, err := s.SearchByEmbedding(ctx, vec(1, 0), tt.opts)
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

	rec, err := s.GetVariantRecord(ctx, "a2-v")
	if err != nil || rec.Scope != "a" || !rec.Timestamp.Equal(t0.Add(-24*time.Hour)) {
		t.Errorf("record = %+v, %v", rec, err)
	}
	found, err := s.FindBySourceURI(ctx, "a", "fs://x/1")
	if err != nil || len(found) != 1 || found[0].UUID != "a1" {
		t.Errorf("FindBySourceURI = %+v, %v", found, err)
	}
	listed, err := s.ListSourceDocuments(ctx, "a", "fs://x/")
	if err != nil || len(listed) != 2 || listed[0].SourceURI != "fs://x/1" || listed[1].SourceModifiedAt.IsZero() {
		t.Errorf("ListSourceDocuments = %+v, %v", listed, err)
	}
	if none, err := s.ListSourceDocuments(ctx, "c", ""); err != nil || len(none) != 0 {
		t.Errorf("unknown scope listed %+v, %v", none, err)
	}
}
