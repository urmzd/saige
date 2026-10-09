package bm25retriever_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/rag/bm25retriever"
	"github.com/urmzd/saige/rag/memstore"
	"github.com/urmzd/saige/rag/types"
)

func TestBM25IndexIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	doc := makeDoc("doc1", "Doc 1", []types.Section{
		makeTextSection("doc1", "sec1", "var1", "zebra crossing"),
	})
	other := makeDoc("doc2", "Doc 2", []types.Section{
		makeTextSection("doc2", "sec2", "var2", "zebra stripes"),
	})
	for _, d := range []*types.Document{doc, other} {
		if err := store.CreateDocument(ctx, d); err != nil {
			t.Fatal(err)
		}
	}

	once := bm25retriever.New(store, nil)
	twice := bm25retriever.New(store, nil)
	for _, d := range []*types.Document{doc, other} {
		if err := once.Index(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []*types.Document{doc, doc, other, doc} {
		if err := twice.Index(ctx, d); err != nil {
			t.Fatal(err)
		}
	}

	want, err := once.Retrieve(ctx, "zebra crossing", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := twice.Retrieve(ctx, "zebra crossing", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d hits, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i].Variant.UUID != want[i].Variant.UUID || got[i].Score != want[i].Score {
			t.Errorf("hit %d = %s %.4f, want %s %.4f", i, got[i].Variant.UUID, got[i].Score, want[i].Variant.UUID, want[i].Score)
		}
	}
}

// seedTagged stores and indexes n documents whose single variant contains
// "apple" repeated so that doc0 ranks first. Documents whose index is in
// tagged carry document-level metadata team=core.
func seedTagged(t *testing.T, ctx context.Context, store *memstore.Store, r *bm25retriever.Retriever, n int, tagged map[int]bool, created time.Time) {
	t.Helper()
	for i := range n {
		uuid := fmt.Sprintf("doc%d", i)
		text := strings.Repeat("apple ", n-i) + "filler words here"
		doc := makeDoc(uuid, uuid, []types.Section{
			makeTextSection(uuid, "sec"+uuid, "var"+uuid, text),
		})
		doc.CreatedAt = created
		if tagged[i] {
			doc.Metadata = map[string]string{"team": "core"}
		}
		if err := store.CreateDocument(ctx, doc); err != nil {
			t.Fatal(err)
		}
		if err := r.Index(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBM25FollowsStoreContract(t *testing.T) {
	created := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		name    string
		opts    *types.SearchOptions
		want    []string
		checkTS bool
	}{
		{
			name: "document metadata filter",
			opts: &types.SearchOptions{Limit: 10, MetadataFilters: []types.MetadataFilter{{Key: "team", Op: types.FilterEq, Value: "core"}}},
			want: []string{"vardoc2", "vardoc3"},
		},
		{
			name: "filter runs before limit",
			opts: &types.SearchOptions{Limit: 2, MetadataFilters: []types.MetadataFilter{{Key: "team", Op: types.FilterEq, Value: "core"}}},
			want: []string{"vardoc2", "vardoc3"},
		},
		{
			name:    "hits carry the document timestamp",
			opts:    &types.SearchOptions{Limit: 1},
			want:    []string{"vardoc0"},
			checkTS: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store := memstore.New()
			r := bm25retriever.New(store, nil)
			seedTagged(t, ctx, store, r, 4, map[int]bool{2: true, 3: true}, created)

			hits, err := r.Retrieve(ctx, "apple", tt.opts)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, h := range hits {
				got = append(got, h.Variant.UUID)
				if tt.checkTS && !h.Timestamp.Equal(created) {
					t.Errorf("timestamp = %v, want %v", h.Timestamp, created)
				}
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("hits = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBM25SkipsVariantsMissingFromStore(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	r := bm25retriever.New(store, nil)
	seedTagged(t, ctx, store, r, 4, nil, time.Time{})
	// doc0 leaves the store without leaving the index.
	if err := store.DeleteDocument(ctx, "doc0"); err != nil {
		t.Fatal(err)
	}
	hits, err := r.Retrieve(ctx, "apple", &types.SearchOptions{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Errorf("got %d hits, want a full page of 2", len(hits))
	}
}

// plainStore hides the optional listing and record lookup interfaces of
// memstore, leaving only the base Store methods.
type plainStore struct{ types.Store }

func TestBM25RebuildIndex(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	seedTagged(t, ctx, store, bm25retriever.New(store, nil), 3, nil, time.Time{})

	tests := []struct {
		name    string
		store   types.Store
		wantErr error
		want    int
	}{
		{name: "listing store", store: store, want: 3},
		{name: "store without listing", store: plainStore{store}, wantErr: bm25retriever.ErrRebuildUnsupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := bm25retriever.New(tt.store, nil)
			err := r.RebuildIndex(ctx)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			// Rebuilding twice must not double-count.
			if err == nil {
				if err := r.RebuildIndex(ctx); err != nil {
					t.Fatal(err)
				}
			}
			hits, err := r.Retrieve(ctx, "apple", nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(hits) != tt.want {
				t.Errorf("got %d hits, want %d", len(hits), tt.want)
			}
		})
	}
}

// failingStore fails every variant lookup with a non-not-found error.
type failingStore struct {
	types.Store
	err error
}

type failingRecordStore struct{ failingStore }

func (f failingRecordStore) GetVariantRecord(context.Context, string) (*types.VariantRecord, error) {
	return nil, f.err
}

type failingBatchStore struct{ failingStore }

func (f failingBatchStore) GetVariantRecords(context.Context, []string) (map[string]*types.VariantRecord, error) {
	return nil, f.err
}

func (f failingStore) GetVariant(context.Context, string) (*types.ContentVariant, *types.Provenance, error) {
	return nil, nil, f.err
}

// countingStore counts batch lookups.
type countingStore struct {
	*memstore.Store
	batches int
}

func (c *countingStore) GetVariantRecords(ctx context.Context, uuids []string) (map[string]*types.VariantRecord, error) {
	c.batches++
	return c.Store.GetVariantRecords(ctx, uuids)
}

func TestBM25RetrieveStoreErrors(t *testing.T) {
	boom := errors.New("connection reset")
	tests := []struct {
		name    string
		wrap    func(types.Store) types.Store
		wantErr error
	}{
		{name: "batch lookup error", wrap: func(s types.Store) types.Store { return failingBatchStore{failingStore{Store: s, err: boom}} }, wantErr: boom},
		{name: "record lookup error", wrap: func(s types.Store) types.Store { return failingRecordStore{failingStore{Store: s, err: boom}} }, wantErr: boom},
		{name: "variant lookup error", wrap: func(s types.Store) types.Store { return failingStore{Store: s, err: boom} }, wantErr: boom},
		{name: "cancelled context", wrap: func(s types.Store) types.Store { return failingStore{Store: s, err: context.Canceled} }, wantErr: context.Canceled},
		{name: "not found is skipped", wrap: func(s types.Store) types.Store {
			return failingRecordStore{failingStore{Store: s, err: types.ErrVariantNotFound}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			mem := memstore.New()
			seedTagged(t, ctx, mem, bm25retriever.New(mem, nil), 3, nil, time.Time{})
			r := bm25retriever.New(tt.wrap(mem), nil)
			if err := r.RebuildIndex(ctx); err != nil && !errors.Is(err, bm25retriever.ErrRebuildUnsupported) {
				t.Fatal(err)
			}
			for i := range 3 {
				uuid := fmt.Sprintf("doc%d", i)
				doc, err := mem.GetDocument(ctx, uuid)
				if err != nil {
					t.Fatal(err)
				}
				if err := r.Index(ctx, doc); err != nil {
					t.Fatal(err)
				}
			}
			hits, err := r.Retrieve(ctx, "apple", nil)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && len(hits) != 0 {
				t.Errorf("got %d hits, want none", len(hits))
			}
		})
	}
}

func TestBM25RetrieveBatchesLookups(t *testing.T) {
	ctx := context.Background()
	store := &countingStore{Store: memstore.New()}
	r := bm25retriever.New(store, nil)
	// 100 documents match "apple" but only the last one passes the filter.
	n := 100
	seedTagged(t, ctx, store.Store, r, n, map[int]bool{n - 1: true}, time.Time{})

	hits, err := r.Retrieve(ctx, "apple", &types.SearchOptions{
		Limit:           1,
		MetadataFilters: []types.MetadataFilter{{Key: "team", Op: types.FilterEq, Value: "core"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Variant.UUID != fmt.Sprintf("vardoc%d", n-1) {
		t.Fatalf("hits = %+v", hits)
	}
	if store.batches > 4 {
		t.Errorf("made %d batch lookups for %d candidates, want at most 4", store.batches, n)
	}
}

func TestBM25RebuildKeepsConcurrentWrites(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	r := bm25retriever.New(store, nil)
	seedTagged(t, ctx, store, r, 50, nil, time.Time{})

	var rebuilder, writers sync.WaitGroup
	stop := make(chan struct{})
	rebuilder.Add(1)
	go func() {
		defer rebuilder.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := r.RebuildIndex(ctx); err != nil {
				t.Error(err)
				return
			}
		}
	}()

	// Writers keep the store and the index in step, as the pipeline does:
	// write the store first, then the index.
	for w := range 4 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for i := range 40 {
				uuid := fmt.Sprintf("w%d-%d", w, i)
				doc := makeDoc(uuid, uuid, []types.Section{
					makeTextSection(uuid, "sec"+uuid, "var"+uuid, "apple pie "+uuid),
				})
				if err := store.CreateDocument(ctx, doc); err != nil {
					t.Error(err)
					return
				}
				if err := r.Index(ctx, doc); err != nil {
					t.Error(err)
					return
				}
				if i%3 == 0 {
					if err := store.DeleteDocument(ctx, uuid); err != nil {
						t.Error(err)
						return
					}
					if err := r.Remove(ctx, uuid); err != nil {
						t.Error(err)
						return
					}
				}
			}
			// Remove a seeded document too.
			old := fmt.Sprintf("doc%d", w)
			if err := store.DeleteDocument(ctx, old); err != nil {
				t.Error(err)
				return
			}
			if err := r.Remove(ctx, old); err != nil {
				t.Error(err)
			}
		}()
	}
	writers.Wait()
	close(stop)
	rebuilder.Wait()

	want := bm25retriever.New(store, nil)
	if err := want.RebuildIndex(ctx); err != nil {
		t.Fatal(err)
	}
	opts := &types.SearchOptions{Limit: 1000}
	wantHits, err := want.Retrieve(ctx, "apple pie", opts)
	if err != nil {
		t.Fatal(err)
	}
	gotHits, err := r.Retrieve(ctx, "apple pie", opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotHits) != len(wantHits) {
		t.Fatalf("got %d hits, want %d", len(gotHits), len(wantHits))
	}
	for i := range gotHits {
		if gotHits[i].Variant.UUID != wantHits[i].Variant.UUID || math.Abs(gotHits[i].Score-wantHits[i].Score) > 1e-9 {
			t.Errorf("hit %d = %s %.6f, want %s %.6f", i, gotHits[i].Variant.UUID, gotHits[i].Score, wantHits[i].Variant.UUID, wantHits[i].Score)
		}
	}
}
