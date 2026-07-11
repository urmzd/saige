package memstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/urmzd/saige/rag/memstore"
	"github.com/urmzd/saige/rag/types"
)

func TestGetVariant(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()

	doc := &types.Document{
		UUID:      "doc1",
		Title:     "Test",
		SourceURI: "http://example.com",
		Sections: []types.Section{{
			UUID:         "sec1",
			DocumentUUID: "doc1",
			Index:        0,
			Heading:      "Intro",
			Variants: []types.ContentVariant{{
				UUID:        "var1",
				SectionUUID: "sec1",
				ContentType: types.ContentText,
				Text:        "hello world",
			}},
		}},
	}
	if err := store.CreateDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}

	v, prov, err := store.GetVariant(ctx, "var1")
	if err != nil {
		t.Fatal(err)
	}
	if v.Text != "hello world" {
		t.Errorf("expected 'hello world', got %q", v.Text)
	}
	if prov.DocumentUUID != "doc1" {
		t.Errorf("expected doc UUID 'doc1', got %q", prov.DocumentUUID)
	}
	if prov.SectionHeading != "Intro" {
		t.Errorf("expected heading 'Intro', got %q", prov.SectionHeading)
	}

	// Not found.
	_, _, err = store.GetVariant(ctx, "nonexistent")
	if err != types.ErrVariantNotFound {
		t.Errorf("expected ErrVariantNotFound, got %v", err)
	}
}

func TestSearchByEmbedding(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()

	doc := &types.Document{
		UUID: "doc1",
		Sections: []types.Section{{
			UUID:         "sec1",
			DocumentUUID: "doc1",
			Variants: []types.ContentVariant{
				{UUID: "v1", SectionUUID: "sec1", ContentType: types.ContentText, Text: "cats", Embedding: []float32{1, 0, 0}},
				{UUID: "v2", SectionUUID: "sec1", ContentType: types.ContentText, Text: "dogs", Embedding: []float32{0, 1, 0}},
				{UUID: "v3", SectionUUID: "sec1", ContentType: types.ContentImage, Text: "image", Embedding: []float32{1, 0, 0}},
			},
		}},
	}
	if err := store.CreateDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}

	// Search without filters.
	hits, err := store.SearchByEmbedding(ctx, []float32{1, 0, 0}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 {
		t.Fatalf("expected 3 hits, got %d", len(hits))
	}

	// Filter by content type.
	hits, err = store.SearchByEmbedding(ctx, []float32{1, 0, 0}, &types.SearchOptions{
		ContentTypes: []types.ContentType{types.ContentText},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected 2 text hits, got %d", len(hits))
	}

	// MinScore filter.
	hits, err = store.SearchByEmbedding(ctx, []float32{1, 0, 0}, &types.SearchOptions{
		MinScore: 0.9,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Only v1 and v3 have cosine similarity 1.0 with query.
	if len(hits) != 2 {
		t.Fatalf("expected 2 hits with min score 0.9, got %d", len(hits))
	}
}

func TestSearchByEmbeddingPopulatesTimestamp(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()

	updated := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	created := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	// Document with UpdatedAt set: timestamp should prefer UpdatedAt.
	updatedDoc := &types.Document{
		UUID:      "doc-updated",
		CreatedAt: created,
		UpdatedAt: updated,
		Sections: []types.Section{{
			UUID: "sec-u", DocumentUUID: "doc-updated",
			Variants: []types.ContentVariant{
				{UUID: "vu", SectionUUID: "sec-u", ContentType: types.ContentText, Embedding: []float32{1, 0}},
			},
		}},
	}
	// Document with only CreatedAt: timestamp should fall back to CreatedAt.
	createdDoc := &types.Document{
		UUID:      "doc-created",
		CreatedAt: created,
		Sections: []types.Section{{
			UUID: "sec-c", DocumentUUID: "doc-created",
			Variants: []types.ContentVariant{
				{UUID: "vc", SectionUUID: "sec-c", ContentType: types.ContentText, Embedding: []float32{1, 0}},
			},
		}},
	}
	if err := store.CreateDocument(ctx, updatedDoc); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateDocument(ctx, createdDoc); err != nil {
		t.Fatal(err)
	}

	hits, err := store.SearchByEmbedding(ctx, []float32{1, 0}, nil)
	if err != nil {
		t.Fatal(err)
	}

	byUUID := make(map[string]types.SearchHit, len(hits))
	for _, h := range hits {
		byUUID[h.Variant.UUID] = h
	}

	if got := byUUID["vu"].Timestamp; !got.Equal(updated) {
		t.Errorf("updated doc hit timestamp = %v, want %v", got, updated)
	}
	if got := byUUID["vc"].Timestamp; !got.Equal(created) {
		t.Errorf("created doc hit timestamp = %v, want %v", got, created)
	}
}

func TestSearchByEmbeddingMetadataFilter(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()

	doc := &types.Document{
		UUID:     "doc1",
		Metadata: map[string]string{"source": "wiki"},
		Sections: []types.Section{{
			UUID:         "sec1",
			DocumentUUID: "doc1",
			Variants: []types.ContentVariant{
				{UUID: "v1", SectionUUID: "sec1", ContentType: types.ContentText, Text: "a", Embedding: []float32{1, 0}, Metadata: map[string]string{"lang": "en"}},
				{UUID: "v2", SectionUUID: "sec1", ContentType: types.ContentText, Text: "b", Embedding: []float32{1, 0}, Metadata: map[string]string{"lang": "fr"}},
			},
		}},
	}
	if err := store.CreateDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}

	hits, err := store.SearchByEmbedding(ctx, []float32{1, 0}, &types.SearchOptions{
		MetadataFilters: []types.MetadataFilter{{Key: "lang", Op: types.FilterEq, Value: "en"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit with lang=en, got %d", len(hits))
	}
	if hits[0].Variant.UUID != "v1" {
		t.Errorf("expected v1, got %q", hits[0].Variant.UUID)
	}
}

func TestReplaceDocument(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()

	old := &types.Document{
		UUID:        "doc-old",
		Fingerprint: "fp-1",
		Sections: []types.Section{{
			UUID: "sec-old", DocumentUUID: "doc-old", Index: 0,
			Variants: []types.ContentVariant{{
				UUID: "var-old", SectionUUID: "sec-old",
				ContentType: types.ContentText, Text: "old",
			}},
		}},
	}
	if err := store.CreateDocument(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreOriginal(ctx, "doc-old", []byte("old bytes")); err != nil {
		t.Fatal(err)
	}

	repl := &types.Document{
		UUID:        "doc-new",
		Fingerprint: "fp-1",
		Sections: []types.Section{{
			UUID: "sec-new", DocumentUUID: "doc-new", Index: 0,
			Variants: []types.ContentVariant{{
				UUID: "var-new", SectionUUID: "sec-new",
				ContentType: types.ContentText, Text: "new",
			}},
		}},
	}
	if err := store.ReplaceDocument(ctx, "doc-old", repl); err != nil {
		t.Fatal(err)
	}

	// Old document, its original bytes, and its fingerprint mapping are gone.
	if _, err := store.GetDocument(ctx, "doc-old"); err != types.ErrDocumentNotFound {
		t.Errorf("old document should be gone, got err: %v", err)
	}
	if _, err := store.GetOriginal(ctx, "doc-old"); err != types.ErrDocumentNotFound {
		t.Errorf("old original should be gone, got err: %v", err)
	}

	// New document is present and reachable by the shared fingerprint.
	if _, err := store.GetDocument(ctx, "doc-new"); err != nil {
		t.Fatalf("new document should exist: %v", err)
	}
	byFP, err := store.FindByFingerprint(ctx, "fp-1")
	if err != nil {
		t.Fatalf("fingerprint should map to new document: %v", err)
	}
	if byFP.UUID != "doc-new" {
		t.Errorf("fingerprint maps to %q, want doc-new", byFP.UUID)
	}
}

func TestReplaceDocumentMissingOld(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()

	doc := &types.Document{UUID: "doc-1", Fingerprint: "fp-x"}
	if err := store.ReplaceDocument(ctx, "nonexistent", doc); err != nil {
		t.Fatalf("replace with missing old doc should still insert: %v", err)
	}
	if _, err := store.GetDocument(ctx, "doc-1"); err != nil {
		t.Errorf("new document should exist: %v", err)
	}
}
