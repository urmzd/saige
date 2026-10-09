package parentretriever_test

import (
	"context"
	"errors"
	"testing"

	"github.com/urmzd/saige/rag/chunker"
	"github.com/urmzd/saige/rag/memstore"
	"github.com/urmzd/saige/rag/parentretriever"
	"github.com/urmzd/saige/rag/types"
)

type mockRetriever struct {
	hits []types.SearchHit
}

func (m *mockRetriever) Retrieve(_ context.Context, _ string, _ *types.SearchOptions) ([]types.SearchHit, error) {
	return m.hits, nil
}

func TestParentRetrieverExpandsSection(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()

	// Create a document with a section that has multiple variants.
	doc := &types.Document{
		UUID:  "doc1",
		Title: "Test Doc",
		Sections: []types.Section{{
			UUID:         "sec1",
			DocumentUUID: "doc1",
			Index:        0,
			Heading:      "Introduction",
			Variants: []types.ContentVariant{
				{UUID: "v1", SectionUUID: "sec1", ContentType: types.ContentText, Text: "First paragraph."},
				{UUID: "v2", SectionUUID: "sec1", ContentType: types.ContentText, Text: "Second paragraph."},
			},
		}},
	}
	if err := store.CreateDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}

	// Mock inner retriever returns a hit for just v1.
	inner := &mockRetriever{
		hits: []types.SearchHit{{
			Variant: types.ContentVariant{UUID: "v1", Text: "First paragraph."},
			Score:   0.9,
			Provenance: types.Provenance{
				DocumentUUID:   "doc1",
				SectionUUID:    "sec1",
				SectionHeading: "Introduction",
			},
		}},
	}

	r := parentretriever.New(inner, store)
	hits, err := r.Retrieve(ctx, "test", nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(hits))
	}

	// Text should be expanded to include both variants.
	if hits[0].Variant.Text != "First paragraph.\n\nSecond paragraph." {
		t.Errorf("expected expanded text, got %q", hits[0].Variant.Text)
	}
}

func TestParentRetrieverDedupes(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()

	doc := &types.Document{
		UUID: "doc1",
		Sections: []types.Section{{
			UUID:         "sec1",
			DocumentUUID: "doc1",
			Variants: []types.ContentVariant{
				{UUID: "v1", SectionUUID: "sec1", ContentType: types.ContentText, Text: "Text A."},
				{UUID: "v2", SectionUUID: "sec1", ContentType: types.ContentText, Text: "Text B."},
			},
		}},
	}
	if err := store.CreateDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}

	// Inner retriever returns two hits from same section.
	inner := &mockRetriever{
		hits: []types.SearchHit{
			{
				Variant:    types.ContentVariant{UUID: "v1", Text: "Text A."},
				Score:      0.9,
				Provenance: types.Provenance{DocumentUUID: "doc1", SectionUUID: "sec1"},
			},
			{
				Variant:    types.ContentVariant{UUID: "v2", Text: "Text B."},
				Score:      0.8,
				Provenance: types.Provenance{DocumentUUID: "doc1", SectionUUID: "sec1"},
			},
		},
	}

	r := parentretriever.New(inner, store)
	hits, err := r.Retrieve(ctx, "test", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Should be deduped to one hit (highest score).
	if len(hits) != 1 {
		t.Fatalf("expected 1 deduped hit, got %d", len(hits))
	}
	if hits[0].Score != 0.9 {
		t.Errorf("expected highest score 0.9, got %f", hits[0].Score)
	}
}

type indexingRetriever struct {
	mockRetriever
	indexed []string
	removed []string
}

func (r *indexingRetriever) Index(_ context.Context, doc *types.Document) error {
	r.indexed = append(r.indexed, doc.UUID)
	return nil
}

func (r *indexingRetriever) Remove(_ context.Context, documentUUID string) error {
	r.removed = append(r.removed, documentUUID)
	return nil
}

func TestParentRetrieverForwardsIndexer(t *testing.T) {
	ctx := context.Background()
	doc := &types.Document{UUID: "d1"}

	tests := []struct {
		name  string
		inner types.Retriever
		want  bool
	}{
		{name: "indexer inner", inner: &indexingRetriever{}, want: true},
		{name: "plain inner", inner: &mockRetriever{}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var r types.Retriever = parentretriever.New(tt.inner, memstore.New())
			indexer, ok := r.(types.Indexer)
			if !ok {
				t.Fatal("parent retriever must implement types.Indexer")
			}
			if err := indexer.Index(ctx, doc); err != nil {
				t.Fatal(err)
			}
			if err := indexer.Remove(ctx, doc.UUID); err != nil {
				t.Fatal(err)
			}
			if inner, ok := tt.inner.(*indexingRetriever); ok != tt.want {
				t.Fatalf("unexpected inner type")
			} else if ok && (len(inner.indexed) != 1 || len(inner.removed) != 1) {
				t.Errorf("forwarded index=%v remove=%v, want one call each", inner.indexed, inner.removed)
			}
		})
	}
}

// sectionStore counts GetSections calls and can fail them.
type sectionStore struct {
	*memstore.Store
	calls int
	err   error
}

func (s *sectionStore) GetSections(ctx context.Context, documentUUID string) ([]types.Section, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.Store.GetSections(ctx, documentUUID)
}

func TestParentRetrieverLoadsSectionsOncePerDocument(t *testing.T) {
	ctx := context.Background()
	doc := &types.Document{
		UUID: "doc1",
		Sections: []types.Section{
			{UUID: "sec1", DocumentUUID: "doc1", Variants: []types.ContentVariant{
				{UUID: "v1", SectionUUID: "sec1", ContentType: types.ContentText, Text: "One."},
			}},
			{UUID: "sec2", DocumentUUID: "doc1", Variants: []types.ContentVariant{
				{UUID: "v2", SectionUUID: "sec2", ContentType: types.ContentText, Text: "Two."},
				{UUID: "v3", SectionUUID: "sec2", ContentType: types.ContentText, Text: "Three."},
			}},
		},
	}
	hit := func(variant, section string, score float64) types.SearchHit {
		return types.SearchHit{
			Variant:    types.ContentVariant{UUID: variant, Text: "child"},
			Score:      score,
			Provenance: types.Provenance{DocumentUUID: "doc1", SectionUUID: section},
		}
	}
	inner := &mockRetriever{hits: []types.SearchHit{hit("v1", "sec1", 0.9), hit("v2", "sec2", 0.8), hit("v3", "sec2", 0.7)}}

	tests := []struct {
		name         string
		failSections bool
		wantPartial  bool
		wantExpanded bool
	}{
		{name: "expands", wantExpanded: true},
		{name: "section load failure is partial", failSections: true, wantPartial: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &sectionStore{Store: memstore.New()}
			if err := store.CreateDocument(ctx, doc); err != nil {
				t.Fatal(err)
			}
			if tt.failSections {
				store.err = errors.New("db down")
			}
			hits, err := parentretriever.New(inner, store).Retrieve(ctx, "q", nil)
			if errors.Is(err, types.ErrPartialSearch) != tt.wantPartial {
				t.Fatalf("err = %v, want partial %v", err, tt.wantPartial)
			}
			if store.calls != 1 {
				t.Errorf("GetSections calls = %d, want 1", store.calls)
			}
			if len(hits) != 2 {
				t.Fatalf("got %d hits, want one per section", len(hits))
			}
			for _, h := range hits {
				expanded := h.Provenance.ExpandedFromVariantUUID == h.Variant.UUID && h.Variant.Text != "child"
				if expanded != tt.wantExpanded {
					t.Errorf("hit %s expanded = %v, want %v (text %q)", h.Variant.UUID, expanded, tt.wantExpanded, h.Variant.Text)
				}
			}
		})
	}
}

type rebuildingRetriever struct {
	mockRetriever
	rebuilt int
}

func (r *rebuildingRetriever) RebuildIndex(context.Context) error {
	r.rebuilt++
	return nil
}

func TestParentRetrieverForwardsRebuild(t *testing.T) {
	inner := &rebuildingRetriever{}
	var r types.Retriever = parentretriever.New(inner, memstore.New())
	rebuilder, ok := r.(types.IndexRebuilder)
	if !ok {
		t.Fatal("parent retriever must implement types.IndexRebuilder")
	}
	if err := rebuilder.RebuildIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	if inner.rebuilt != 1 {
		t.Errorf("inner rebuilt %d times, want 1", inner.rebuilt)
	}
	if err := parentretriever.New(&mockRetriever{}, memstore.New()).RebuildIndex(context.Background()); err != nil {
		t.Errorf("rebuild without an inner index = %v, want nil", err)
	}
}

// TestParentRetrieverSkipsSingleChunkSection checks that a hit whose section
// holds only its own chunk, as the recursive chunker produces, is returned
// unchanged and not recorded as an expansion.
func TestParentRetrieverSkipsSingleChunkSection(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	doc, err := chunker.NewRecursive(&chunker.Config{MaxTokens: 8}).Chunk(ctx, &types.Document{
		UUID: "doc1",
		Sections: []types.Section{{
			UUID: "sec1", DocumentUUID: "doc1",
			Variants: []types.ContentVariant{{UUID: "v1", SectionUUID: "sec1", ContentType: types.ContentText,
				Text: "Alpha beta gamma delta epsilon.\n\nZeta eta theta iota kappa lambda.\n\nMu nu xi omicron pi rho."}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Sections) < 2 {
		t.Fatalf("chunker produced %d sections, want several single-chunk sections", len(doc.Sections))
	}
	if err := store.CreateDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}
	chunk := doc.Sections[1].Variants[0]
	inner := &mockRetriever{hits: []types.SearchHit{{
		Variant:    types.ContentVariant{UUID: chunk.UUID, Text: chunk.Text},
		Score:      0.9,
		Provenance: types.Provenance{DocumentUUID: "doc1", SectionUUID: doc.Sections[1].UUID},
	}}}
	hits, err := parentretriever.New(inner, store).Retrieve(ctx, "q", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Variant.Text != chunk.Text {
		t.Fatalf("hits = %+v, want the chunk unchanged", hits)
	}
	if got := hits[0].Provenance.ExpandedFromVariantUUID; got != "" {
		t.Fatalf("ExpandedFromVariantUUID = %q, want empty: nothing was expanded", got)
	}
}
