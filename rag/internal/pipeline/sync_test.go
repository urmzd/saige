package pipeline_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/urmzd/saige/rag/internal/pipeline"
	"github.com/urmzd/saige/rag/memstore"
	"github.com/urmzd/saige/rag/source"
	"github.com/urmzd/saige/rag/types"
)

// listSource returns a fixed listing.
type listSource struct{ docs []types.RawDocument }

func (s *listSource) Fetch(context.Context) ([]types.RawDocument, error) { return s.docs, nil }

// bareStore hides memstore's optional source interfaces.
type bareStore struct{ types.Store }

func TestPipelineSyncSource(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(24 * time.Hour)

	pipe, store := newScopedPipeline("", types.DedupSkip)
	syncer := pipe.(types.SourceSyncer)
	src := &listSource{docs: []types.RawDocument{
		{SourceURI: "fs://docs/a.md", Data: []byte("alpha v1"), SourceModifiedAt: t0},
		{SourceURI: "fs://docs/b.md", Data: []byte("beta v1"), SourceModifiedAt: t0},
		{SourceURI: "fs://docs/c.md", Data: []byte("gamma v1"), SourceModifiedAt: t0},
	}}

	first, err := syncer.SyncSource(ctx, src, types.SyncOptions{Scope: "team"})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Created) != 3 || !first.Cursor.Equal(t0) {
		t.Fatalf("first sync = %+v", first)
	}
	uuidOf := func(uri string) string {
		docs, err := store.FindBySourceURI(ctx, "team", uri)
		if err != nil || len(docs) != 1 {
			t.Fatalf("FindBySourceURI(%s) = %+v, %v", uri, docs, err)
		}
		return docs[0].UUID
	}
	aUUID, bUUID, cUUID := uuidOf("fs://docs/a.md"), uuidOf("fs://docs/b.md"), uuidOf("fs://docs/c.md")

	// a changes, b is untouched, c disappears, d is new.
	src.docs = []types.RawDocument{
		{SourceURI: "fs://docs/a.md", Data: []byte("alpha v2"), SourceModifiedAt: t1},
		{SourceURI: "fs://docs/b.md", Data: []byte("beta v1"), SourceModifiedAt: t0},
		{SourceURI: "fs://docs/d.md", Data: []byte("delta v1"), SourceModifiedAt: t1},
	}

	tests := []struct {
		name string
		opts types.SyncOptions
		want func(t *testing.T, r *types.SyncResult)
	}{
		{
			name: "without prune nothing is deleted",
			opts: types.SyncOptions{Scope: "team", Since: first.Cursor},
			want: func(t *testing.T, r *types.SyncResult) {
				if !slices.Equal(r.Updated, []string{aUUID}) || !slices.Equal(r.Unchanged, []string{bUUID}) ||
					len(r.Created) != 1 || len(r.Pruned) != 0 || !r.Cursor.Equal(t1) {
					t.Errorf("result = %+v", r)
				}
				if doc, err := store.GetDocument(ctx, aUUID); err != nil || doc.Sections[0].Variants[0].Text != "alpha v2" {
					t.Errorf("a not updated in place: %v", err)
				}
				if _, err := store.GetDocument(ctx, cUUID); err != nil {
					t.Errorf("c deleted without prune")
				}
			},
		},
		{
			name: "prune deletes URIs the source dropped",
			opts: types.SyncOptions{Scope: "team", Since: t1, Prune: true, PrunePrefix: "fs://docs/"},
			want: func(t *testing.T, r *types.SyncResult) {
				if len(r.Updated)+len(r.Created) != 0 || len(r.Unchanged) != 3 || !slices.Equal(r.Pruned, []string{cUUID}) {
					t.Errorf("result = %+v", r)
				}
				if _, err := store.GetDocument(ctx, cUUID); !errors.Is(err, types.ErrDocumentNotFound) {
					t.Errorf("c survived prune: %v", err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := syncer.SyncSource(ctx, src, tt.opts)
			if err != nil {
				t.Fatal(err)
			}
			tt.want(t, r)
		})
	}

	// Another scope sees none of it.
	other, err := syncer.SyncSource(ctx, &listSource{}, types.SyncOptions{Scope: "other", Prune: true})
	if err != nil || len(other.Pruned) != 0 {
		t.Fatalf("sync of an empty source in another scope pruned %v (%v)", other.Pruned, err)
	}
}

func TestPipelineSyncSourceErrors(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name     string
		store    types.Store
		scope    string
		src      []types.RawDocument
		wantErr  error
		wantFail int
	}{
		{name: "store without source listing", store: bareStore{memstore.New()}, wantErr: types.ErrSyncUnsupported},
		{name: "fixed scope mismatch", store: memstore.New(), scope: "other", wantErr: types.ErrScopeMismatch},
		{
			name: "per-URI failures are reported, others still sync", store: memstore.New(),
			src: []types.RawDocument{
				{SourceURI: "", Data: []byte("no uri")},
				{SourceURI: "u://1", Data: []byte("one")},
				{SourceURI: "u://1", Data: []byte("one again")},
				{SourceURI: "u://2", Data: []byte("two"), Scope: "elsewhere"},
			},
			wantFail: 3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pipe := pipeline.New(pipeline.Config{
				Store: tt.store, ContentExtractor: &uniqueExtractor{}, Scope: "fixed",
				Retrievers: []types.Retriever{fixedListRetriever{}},
			})
			res, err := pipe.(types.SourceSyncer).SyncSource(ctx, &listSource{docs: tt.src}, types.SyncOptions{Scope: tt.scope})
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if res == nil || len(res.Failed) != tt.wantFail || len(res.Created) != 1 {
				t.Fatalf("result = %+v", res)
			}
			var se *types.SyncError
			if !errors.As(err, &se) {
				t.Errorf("err = %v, want a joined SyncError", err)
			}
		})
	}
}

// failingReplaceStore fails ReplaceDocument for documents of failURI.
type failingReplaceStore struct {
	*memstore.Store
	failURI string
}

func (s *failingReplaceStore) ReplaceDocument(ctx context.Context, oldUUID string, doc *types.Document) error {
	if s.failURI != "" && doc.SourceURI == s.failURI {
		return errors.New("replace failed")
	}
	return s.Store.ReplaceDocument(ctx, oldUUID, doc)
}

func TestPipelineSyncSourceFailedUpdateIsRetried(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	t1, t2 := t0.Add(time.Hour), t0.Add(2*time.Hour)

	store := &failingReplaceStore{Store: memstore.New()}
	pipe := pipeline.New(pipeline.Config{
		Store: store, ContentExtractor: &uniqueExtractor{},
		Retrievers: []types.Retriever{fixedListRetriever{}},
	})
	syncer := pipe.(types.SourceSyncer)
	src := &listSource{docs: []types.RawDocument{
		{SourceURI: "u://a", Data: []byte("a v1"), SourceModifiedAt: t0},
		{SourceURI: "u://b", Data: []byte("b v1"), SourceModifiedAt: t0},
	}}
	first, err := syncer.SyncSource(ctx, src, types.SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		docs       []types.RawDocument
		failURI    string
		wantCursor time.Time
		wantUpdate int
		wantFailed int
	}{
		{
			// a fails at t1 while b succeeds at t2: the cursor must stay
			// before t1 so the next sync does not count a as unchanged.
			name: "failed update holds the cursor", failURI: "u://a",
			docs: []types.RawDocument{
				{SourceURI: "u://a", Data: []byte("a v2"), SourceModifiedAt: t1},
				{SourceURI: "u://b", Data: []byte("b v2"), SourceModifiedAt: t2},
			},
			wantCursor: t1.Add(-time.Nanosecond), wantUpdate: 1, wantFailed: 1,
		},
		{
			name: "next sync retries the failed update",
			docs: []types.RawDocument{
				{SourceURI: "u://a", Data: []byte("a v2"), SourceModifiedAt: t1},
				{SourceURI: "u://b", Data: []byte("b v2"), SourceModifiedAt: t2},
			},
			wantCursor: t2, wantUpdate: 1,
		},
	}
	since := first.Cursor
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store.failURI = tt.failURI
			src.docs = tt.docs
			r, _ := syncer.SyncSource(ctx, src, types.SyncOptions{Since: since})
			if !r.Cursor.Equal(tt.wantCursor) || len(r.Updated) != tt.wantUpdate || len(r.Failed) != tt.wantFailed {
				t.Fatalf("result = %+v, want cursor %v, %d updated, %d failed", r, tt.wantCursor, tt.wantUpdate, tt.wantFailed)
			}
			since = r.Cursor
		})
	}
}

func TestPipelineSyncSourcePruneKeepsRejectedURIs(t *testing.T) {
	ctx := context.Background()
	pipe, store := newScopedPipeline("", types.DedupSkip)
	syncer := pipe.(types.SourceSyncer)
	src := &listSource{docs: []types.RawDocument{{SourceURI: "u://a", Data: []byte("a v1")}}}
	if _, err := syncer.SyncSource(ctx, src, types.SyncOptions{Scope: "team"}); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		scope string
	}{
		{"document names a foreign scope", "elsewhere"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src.docs = []types.RawDocument{{SourceURI: "u://a", Data: []byte("a v2"), Scope: tt.scope}}
			r, err := syncer.SyncSource(ctx, src, types.SyncOptions{Scope: "team", Prune: true})
			if !errors.Is(err, types.ErrScopeMismatch) {
				t.Fatalf("err = %v, want ErrScopeMismatch", err)
			}
			if len(r.Pruned) != 0 {
				t.Errorf("pruned %v, want none", r.Pruned)
			}
			docs, err := store.FindBySourceURI(ctx, "team", "u://a")
			if err != nil || len(docs) != 1 {
				t.Errorf("stored documents for u://a = %+v, %v; want the original", docs, err)
			}
		})
	}
}

// TestPipelineSyncSourceIdenticalContentSettles checks that two URIs with
// the same bytes settle after one sync under either dedup behavior: later
// syncs create nothing and the first URI keeps its document.
func TestPipelineSyncSourceIdenticalContentSettles(t *testing.T) {
	for _, dedup := range []types.DedupBehavior{types.DedupSkip, types.DedupReplace} {
		t.Run(fmt.Sprintf("dedup=%d", dedup), func(t *testing.T) {
			ctx := context.Background()
			pipe, store := newScopedPipeline("", dedup)
			syncer := pipe.(types.SourceSyncer)
			src := &listSource{docs: []types.RawDocument{
				{SourceURI: "fs://a.md", Data: []byte("same bytes")},
				{SourceURI: "fs://b.md", Data: []byte("same bytes")},
			}}

			var firstA string
			for i := range 3 {
				r, err := syncer.SyncSource(ctx, src, types.SyncOptions{})
				if err != nil {
					t.Fatalf("sync %d: %v", i, err)
				}
				wantCreated := 0
				if i == 0 {
					wantCreated = 1
				}
				if len(r.Created) != wantCreated || len(r.Unchanged) != 2-wantCreated {
					t.Errorf("sync %d = %+v, want %d created", i, r, wantCreated)
				}
				a, err := store.FindBySourceURI(ctx, "", "fs://a.md")
				if err != nil || len(a) != 1 {
					t.Fatalf("sync %d: documents for a = %+v, %v", i, a, err)
				}
				if i == 0 {
					firstA = a[0].UUID
				} else if a[0].UUID != firstA {
					t.Errorf("sync %d: a moved from %s to %s", i, firstA, a[0].UUID)
				}
			}
		})
	}
}

// TestPipelineSyncSourceDetectsEditWithRestoredMtime edits a file and puts
// its old modification time back, as cp -p and rsync -a do. The bytes are in
// hand, so the fingerprint must catch the change even though the time is not
// after the cursor.
func TestPipelineSyncSourceDetectsEditWithRestoredMtime(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "a.md")
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.WriteFile(path, []byte("alpha v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	pipe, store := newScopedPipeline("", types.DedupSkip)
	syncer := pipe.(types.SourceSyncer)
	src := &source.Filesystem{Dir: dir}
	first, err := syncer.SyncSource(ctx, src, types.SyncOptions{})
	if err != nil || len(first.Created) != 1 {
		t.Fatalf("first sync = %+v, %v", first, err)
	}

	if err := os.WriteFile(path, []byte("alpha v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	second, err := syncer.SyncSource(ctx, src, types.SyncOptions{Since: first.Cursor})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(second.Updated, first.Created) {
		t.Fatalf("second sync = %+v, want %v updated", second, first.Created)
	}
	doc, err := store.GetDocument(ctx, first.Created[0])
	if err != nil || doc.Sections[0].Variants[0].Text != "alpha v2" {
		t.Fatalf("document not updated: %v", err)
	}
}

// TestPipelineSyncSourceTrustsTimeWithoutBytes keeps the cursor shortcut for
// a source that sends no bytes for an item it reports as not modified.
func TestPipelineSyncSourceTrustsTimeWithoutBytes(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	pipe, _ := newScopedPipeline("", types.DedupSkip)
	syncer := pipe.(types.SourceSyncer)
	src := &listSource{docs: []types.RawDocument{{SourceURI: "u://a", Data: []byte("a v1"), SourceModifiedAt: t0}}}
	first, err := syncer.SyncSource(ctx, src, types.SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	src.docs = []types.RawDocument{{SourceURI: "u://a", SourceModifiedAt: t0}}
	r, err := syncer.SyncSource(ctx, src, types.SyncOptions{Since: first.Cursor})
	if err != nil || !slices.Equal(r.Unchanged, first.Created) {
		t.Fatalf("result = %+v, %v", r, err)
	}
}

// filteringSource returns a fixed listing and a fixed skip list.
type filteringSource struct {
	listSource
	skipped []types.SkippedSource
}

func (s *filteringSource) FetchWithSkips(context.Context) ([]types.RawDocument, []types.SkippedSource, error) {
	return s.docs, s.skipped, nil
}

// TestPipelineSyncSourcePruneKeepsSkippedURIs: a URI the source skipped by
// rule is reported and kept, not pruned as absent. Only a URI that is gone
// is pruned.
func TestPipelineSyncSourcePruneKeepsSkippedURIs(t *testing.T) {
	ctx := context.Background()
	pipe, store := newScopedPipeline("", types.DedupSkip)
	syncer := pipe.(types.SourceSyncer)
	src := &filteringSource{listSource: listSource{docs: []types.RawDocument{
		{SourceURI: "fs://r/a.md", Data: []byte("a")},
		{SourceURI: "fs://r/.env", Data: []byte("SECRET=1")},
		{SourceURI: "fs://r/vendor/x.md", Data: []byte("x")},
		{SourceURI: "fs://r/gone.md", Data: []byte("gone")},
	}}}
	first, err := syncer.SyncSource(ctx, src, types.SyncOptions{})
	if err != nil || len(first.Created) != 4 {
		t.Fatalf("first sync = %+v, %v", first, err)
	}

	src.docs = []types.RawDocument{{SourceURI: "fs://r/a.md", Data: []byte("a")}}
	src.skipped = []types.SkippedSource{
		{SourceURI: "fs://r/.env", Reason: "secret"},
		{SourceURI: "fs://r/vendor/", Prefix: true, Reason: "excluded"},
	}
	r, err := syncer.SyncSource(ctx, src, types.SyncOptions{Prune: true, PrunePrefix: "fs://r/"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Pruned) != 1 || !slices.Equal(r.Skipped, src.skipped) {
		t.Fatalf("result = %+v, want only gone.md pruned and the skips reported", r)
	}
	for _, uri := range []string{"fs://r/.env", "fs://r/vendor/x.md"} {
		if docs, err := store.FindBySourceURI(ctx, "", uri); err != nil || len(docs) != 1 {
			t.Errorf("%s: %v, %v; a skipped URI must not be pruned", uri, docs, err)
		}
	}
	if docs, _ := store.FindBySourceURI(ctx, "", "fs://r/gone.md"); len(docs) != 0 {
		t.Error("gone.md survived prune")
	}
}
