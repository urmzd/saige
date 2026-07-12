package bm25retriever_test

import (
	"context"
	"testing"

	"github.com/urmzd/saige/rag/bm25retriever"
	"github.com/urmzd/saige/rag/memstore"
	"github.com/urmzd/saige/rag/types"
)

func makeDoc(uuid, title string, sections []types.Section) *types.Document {
	return &types.Document{
		UUID:     uuid,
		Title:    title,
		Sections: sections,
	}
}

func makeTextSection(docUUID, secUUID, varUUID, text string) types.Section {
	return types.Section{
		UUID:         secUUID,
		DocumentUUID: docUUID,
		Variants: []types.ContentVariant{{
			UUID:        varUUID,
			SectionUUID: secUUID,
			ContentType: types.ContentText,
			Text:        text,
		}},
	}
}

func TestBM25IndexAndSearch(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()

	doc1 := makeDoc("doc1", "Doc 1", []types.Section{
		makeTextSection("doc1", "sec1", "var1", "the quick brown fox jumps over the lazy dog"),
	})
	doc2 := makeDoc("doc2", "Doc 2", []types.Section{
		makeTextSection("doc2", "sec2", "var2", "the lazy cat sleeps all day long"),
	})
	doc3 := makeDoc("doc3", "Doc 3", []types.Section{
		makeTextSection("doc3", "sec3", "var3", "a fast brown fox runs through the forest"),
	})

	for _, doc := range []*types.Document{doc1, doc2, doc3} {
		if err := store.CreateDocument(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}

	r := bm25retriever.New(store, nil)
	for _, doc := range []*types.Document{doc1, doc2, doc3} {
		if err := r.Index(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}

	// Search for "brown fox".
	hits, err := r.Retrieve(ctx, "brown fox", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 2 {
		t.Fatalf("expected at least 2 hits, got %d", len(hits))
	}
	// Both doc1 and doc3 contain "brown fox": they should be top results.
	topUUIDs := map[string]bool{hits[0].Variant.UUID: true, hits[1].Variant.UUID: true}
	if !topUUIDs["var1"] || !topUUIDs["var3"] {
		t.Errorf("expected var1 and var3 in top results, got %v", topUUIDs)
	}

	// Search for "lazy": should match doc1 and doc2.
	hits, err = r.Retrieve(ctx, "lazy", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 2 {
		t.Fatalf("expected at least 2 hits for 'lazy', got %d", len(hits))
	}
}

func TestBM25Remove(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()

	doc := makeDoc("doc1", "Doc 1", []types.Section{
		makeTextSection("doc1", "sec1", "var1", "unique term xylophone"),
	})
	if err := store.CreateDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}

	r := bm25retriever.New(store, nil)
	if err := r.Index(ctx, doc); err != nil {
		t.Fatal(err)
	}

	hits, _ := r.Retrieve(ctx, "xylophone", nil)
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit before remove, got %d", len(hits))
	}

	if err := r.Remove(ctx, "doc1"); err != nil {
		t.Fatal(err)
	}

	hits, _ = r.Retrieve(ctx, "xylophone", nil)
	if len(hits) != 0 {
		t.Fatalf("expected 0 hits after remove, got %d", len(hits))
	}
}

func TestBM25MetadataFilter(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()

	doc := makeDoc("doc1", "Doc 1", []types.Section{{
		UUID:         "sec1",
		DocumentUUID: "doc1",
		Variants: []types.ContentVariant{{
			UUID:        "var1",
			SectionUUID: "sec1",
			ContentType: types.ContentText,
			Text:        "hello world test",
			Metadata:    map[string]string{"lang": "en"},
		}},
	}})
	if err := store.CreateDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}

	r := bm25retriever.New(store, nil)
	if err := r.Index(ctx, doc); err != nil {
		t.Fatal(err)
	}

	// Filter that should match.
	hits, _ := r.Retrieve(ctx, "hello", &types.SearchOptions{
		MetadataFilters: []types.MetadataFilter{{Key: "lang", Op: types.FilterEq, Value: "en"}},
	})
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit with matching filter, got %d", len(hits))
	}

	// Filter that should not match.
	hits, _ = r.Retrieve(ctx, "hello", &types.SearchOptions{
		MetadataFilters: []types.MetadataFilter{{Key: "lang", Op: types.FilterEq, Value: "fr"}},
	})
	if len(hits) != 0 {
		t.Fatalf("expected 0 hits with non-matching filter, got %d", len(hits))
	}
}
