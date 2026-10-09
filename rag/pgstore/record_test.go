package pgstore

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/urmzd/saige/rag/bm25retriever"
	"github.com/urmzd/saige/rag/types"
)

func TestGetVariantRecordAndListDocuments(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := NewStore(pool, nil)

	docs := []*types.Document{
		singleSectionDoc("rec-a", "fp-rec-a", map[string]string{"team": "core"},
			types.ContentVariant{UUID: "rec-a-v", ContentType: types.ContentText, Text: "alpha", Metadata: map[string]string{"lang": "en"}}),
		singleSectionDoc("rec-b", "fp-rec-b", nil,
			types.ContentVariant{UUID: "rec-b-v", ContentType: types.ContentText, Text: "beta"}),
	}
	for _, d := range docs {
		if err := store.CreateDocument(ctx, d); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name     string
		variant  string
		wantErr  error
		wantDoc  string
		wantMeta map[string]string
	}{
		{name: "with document metadata", variant: "rec-a-v", wantDoc: "rec-a", wantMeta: map[string]string{"team": "core"}},
		{name: "without document metadata", variant: "rec-b-v", wantDoc: "rec-b"},
		{name: "missing variant", variant: "nope", wantErr: types.ErrVariantNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, err := store.GetVariantRecord(ctx, tt.variant)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if rec.Provenance.DocumentUUID != tt.wantDoc || rec.Variant.UUID != tt.variant {
				t.Errorf("record = %+v", rec)
			}
			if rec.Timestamp.IsZero() {
				t.Error("timestamp must be set")
			}
			if rec.DocumentMetadata["team"] != tt.wantMeta["team"] {
				t.Errorf("document metadata = %v, want %v", rec.DocumentMetadata, tt.wantMeta)
			}
		})
	}

	uuids, err := store.ListDocumentUUIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(uuids, []string{"rec-a", "rec-b"}) {
		t.Errorf("listed %v, want [rec-a rec-b]", uuids)
	}
}

func TestBM25RebuildFromPostgres(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := NewStore(pool, nil)
	doc := singleSectionDoc("bm25-a", "fp-bm25-a", map[string]string{"team": "core"},
		types.ContentVariant{UUID: "bm25-a-v", ContentType: types.ContentText, Text: "the okapi grazes"})
	if err := store.CreateDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}

	r := bm25retriever.New(store, nil)
	if err := r.RebuildIndex(ctx); err != nil {
		t.Fatal(err)
	}
	hits, err := r.Retrieve(ctx, "okapi", &types.SearchOptions{
		MetadataFilters: []types.MetadataFilter{{Key: "team", Op: types.FilterEq, Value: "core"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Timestamp.IsZero() {
		t.Fatalf("hits = %+v, want one dated hit", hits)
	}
}

func TestGetVariantRecordsBatch(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := NewStore(pool, nil)
	for _, d := range []*types.Document{
		singleSectionDoc("batch-a", "fp-batch-a", map[string]string{"team": "core"},
			types.ContentVariant{UUID: "batch-a-v", ContentType: types.ContentText, Text: "alpha"}),
		singleSectionDoc("batch-b", "fp-batch-b", nil,
			types.ContentVariant{UUID: "batch-b-v", ContentType: types.ContentText, Text: "beta"}),
	} {
		if err := store.CreateDocument(ctx, d); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name  string
		uuids []string
		want  []string
	}{
		{name: "empty", uuids: nil},
		{name: "all present", uuids: []string{"batch-a-v", "batch-b-v"}, want: []string{"batch-a-v", "batch-b-v"}},
		{name: "missing omitted", uuids: []string{"batch-a-v", "nope"}, want: []string{"batch-a-v"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recs, err := store.GetVariantRecords(ctx, tt.uuids)
			if err != nil {
				t.Fatal(err)
			}
			if len(recs) != len(tt.want) {
				t.Fatalf("got %d records, want %d", len(recs), len(tt.want))
			}
			for _, u := range tt.want {
				rec, ok := recs[u]
				if !ok || rec.Variant.UUID != u || rec.Timestamp.IsZero() {
					t.Errorf("record %s = %+v", u, rec)
				}
			}
			if rec := recs["batch-a-v"]; rec != nil && rec.DocumentMetadata["team"] != "core" {
				t.Errorf("document metadata = %v", rec.DocumentMetadata)
			}
		})
	}
}
