package pgstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxvector "github.com/pgvector/pgvector-go/pgx"

	"github.com/urmzd/saige/rag/extractor"
	"github.com/urmzd/saige/rag/internal/pipeline"
	"github.com/urmzd/saige/rag/types"
)

// indexOnlyPool opens a second pool on the test database whose sessions
// disable sequential scans and explicit sorts, so the planner orders by the
// HNSW index even on a small table. The settings are session parameters of
// this pool only; other connections to the database keep the defaults. Call
// it after testPool, which skips the test when no database is configured.
func indexOnlyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(os.Getenv("SAIGE_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["enable_seqscan"] = "off"
	cfg.ConnConfig.RuntimeParams["enable_sort"] = "off"
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return pgxvector.RegisterTypes(ctx, conn)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestFilteredSearchUsesIterativeScan forces the HNSW index path and checks
// that a selective filter still fills the limit. 500 non-matching rows sit
// nearer the query than the 5 matching rows, far beyond ef_search=40.
func TestFilteredSearchUsesIterativeScan(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	search := indexOnlyPool(t)

	writer := NewStore(pool, nil)
	variants := make([]types.ContentVariant, 0, 505)
	for i := 0; i < 500; i++ {
		variants = append(variants, types.ContentVariant{
			UUID: fmt.Sprintf("noise-%03d", i), ContentType: types.ContentText, MIMEType: "text/plain",
			Text: "noise", Embedding: vec(1, float32(i)*0.001), Metadata: map[string]string{"lang": "rust"},
		})
	}
	for j := 0; j < 5; j++ {
		variants = append(variants, types.ContentVariant{
			UUID: fmt.Sprintf("match-%d", j), ContentType: types.ContentText, MIMEType: "text/plain",
			Text: "match", Embedding: vec(1, 2.0+0.2*float32(j)), Metadata: map[string]string{"lang": "go"},
		})
	}
	if err := writer.CreateDocument(ctx, singleSectionDoc("doc-hnsw", "fp-hnsw", nil, variants...)); err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}

	opts := &types.SearchOptions{
		Limit:           3,
		MetadataFilters: []types.MetadataFilter{{Key: "lang", Op: types.FilterEq, Value: "go"}},
	}
	tests := []struct {
		name    string
		mode    IterativeScan
		wantMin int
		wantMax int
	}{
		// Without iterative scans the index path filters only the first
		// ef_search candidates, all noise; this proves the index path ran.
		{name: "off", mode: IterativeScanOff, wantMin: 0, wantMax: 2},
		{name: "auto", mode: IterativeScanAuto, wantMin: 3, wantMax: 3},
		{name: "strict", mode: IterativeScanStrict, wantMin: 3, wantMax: 3},
		{name: "relaxed", mode: IterativeScanRelaxed, wantMin: 3, wantMax: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewStore(search, nil, WithIterativeScan(tt.mode))
			hits, err := store.SearchByEmbedding(ctx, vec(1, 0), opts)
			if err != nil {
				t.Fatalf("SearchByEmbedding: %v", err)
			}
			if len(hits) < tt.wantMin || len(hits) > tt.wantMax {
				t.Fatalf("got %d hits, want %d..%d", len(hits), tt.wantMin, tt.wantMax)
			}
			for i, hit := range hits {
				if hit.Variant.Metadata["lang"] != "go" {
					t.Errorf("hit %d lang = %q, want go", i, hit.Variant.Metadata["lang"])
				}
				if i > 0 && hit.Score > hits[i-1].Score {
					t.Errorf("hits out of score order at %d", i)
				}
			}
		})
	}

	// Settings are transaction-local: a pooled connection keeps the defaults.
	var setting string
	if err := search.QueryRow(ctx, `SELECT current_setting('hnsw.iterative_scan')`).Scan(&setting); err != nil {
		t.Fatal(err)
	}
	if setting != "off" {
		t.Errorf("hnsw.iterative_scan leaked to the pool: %q", setting)
	}
}

func TestCreateDocumentDuplicateFingerprint(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := NewStore(pool, nil)

	first := singleSectionDoc("doc-a", "fp-dup", nil)
	if err := store.CreateDocument(ctx, first); err != nil {
		t.Fatal(err)
	}
	err := store.CreateDocument(ctx, singleSectionDoc("doc-b", "fp-dup", nil))
	if !errors.Is(err, types.ErrDuplicateDocument) {
		t.Fatalf("expected ErrDuplicateDocument, got %v", err)
	}
	err = store.ReplaceDocument(ctx, "missing", singleSectionDoc("doc-c", "fp-dup", nil))
	if !errors.Is(err, types.ErrDuplicateDocument) {
		t.Fatalf("replace: expected ErrDuplicateDocument, got %v", err)
	}
}

func TestConcurrentDuplicateIngest(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := NewStore(pool, nil)
	pipe := pipeline.New(pipeline.Config{Store: store, ContentExtractor: &extractor.PlainText{}})
	raw := &types.RawDocument{SourceURI: "test://race", MIMEType: "text/plain", Data: []byte("same content")}

	const workers = 16
	results := make([]*types.IngestResult, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = pipe.Ingest(ctx, raw)
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
		if results[i].DocumentUUID != results[0].DocumentUUID {
			t.Errorf("worker %d got %s, worker 0 got %s", i, results[i].DocumentUUID, results[0].DocumentUUID)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM rag_document`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("stored %d documents, want 1", n)
	}
}

func TestIngestCleansNULAndInvalidUTF8(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := NewStore(pool, nil)
	pipe := pipeline.New(pipeline.Config{Store: store, ContentExtractor: extractor.NewAuto()})

	result, err := pipe.Ingest(ctx, &types.RawDocument{
		SourceURI: "test://dirty",
		MIMEType:  "text/plain",
		Data:      []byte("a\x00b\xffc"),
		Metadata:  map[string]string{"note": "has\x00nul"},
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	doc, err := store.GetDocument(ctx, result.DocumentUUID)
	if err != nil {
		t.Fatal(err)
	}
	texts := []string{doc.Title, doc.Metadata["note"]}
	for _, sec := range doc.Sections {
		texts = append(texts, sec.Heading)
		for _, v := range sec.Variants {
			texts = append(texts, v.Text, v.Metadata["note"])
		}
	}
	for _, s := range texts {
		if !utf8.ValidString(s) || strings.IndexByte(s, 0) >= 0 {
			t.Errorf("stored text is not clean: %q", s)
		}
	}
}

// TestPipelineUpdateKeepsDocumentUUID replaces a stored document in place
// through the pipeline: the delete and re-insert under the same UUID must
// happen in one transaction, keep the UUID, drop the old variants, move the
// fingerprint to the new content, and leave the original bytes writable.
func TestPipelineUpdateKeepsDocumentUUID(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := NewStore(pool, nil)
	pipe := pipeline.New(pipeline.Config{Store: store, ContentExtractor: extractor.NewAuto(), StoreOriginals: true})

	first, err := pipe.Ingest(ctx, &types.RawDocument{
		SourceURI: "test://update", MIMEType: "text/plain", Data: []byte("original content"),
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	before, err := store.GetDocument(ctx, first.DocumentUUID)
	if err != nil {
		t.Fatal(err)
	}
	var oldVariants []string
	for _, sec := range before.Sections {
		for _, v := range sec.Variants {
			oldVariants = append(oldVariants, v.UUID)
		}
	}
	if len(oldVariants) == 0 {
		t.Fatal("ingest stored no variants")
	}

	updated, err := pipe.Update(ctx, first.DocumentUUID, &types.RawDocument{
		SourceURI: "test://update", MIMEType: "text/plain", Data: []byte("updated content"),
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.DocumentUUID != first.DocumentUUID {
		t.Fatalf("update changed the UUID: got %s, want %s", updated.DocumentUUID, first.DocumentUUID)
	}

	after, err := store.GetDocument(ctx, first.DocumentUUID)
	if err != nil {
		t.Fatalf("GetDocument after update: %v", err)
	}
	var texts []string
	for _, sec := range after.Sections {
		for _, v := range sec.Variants {
			texts = append(texts, v.Text)
		}
	}
	if joined := strings.Join(texts, " "); !strings.Contains(joined, "updated content") || strings.Contains(joined, "original content") {
		t.Errorf("variant texts after update = %q, want only the new content", texts)
	}
	for _, id := range oldVariants {
		if _, _, err := store.GetVariant(ctx, id); !errors.Is(err, types.ErrVariantNotFound) {
			t.Errorf("old variant %s still readable: err = %v", id, err)
		}
	}

	if after.Fingerprint == before.Fingerprint {
		t.Fatal("fingerprint did not change with the content")
	}
	owner, err := store.FindByFingerprint(ctx, after.Fingerprint)
	if err != nil || owner.UUID != first.DocumentUUID {
		t.Errorf("FindByFingerprint(new) = %v, %v; want %s", owner, err, first.DocumentUUID)
	}
	if _, err := store.FindByFingerprint(ctx, before.Fingerprint); !errors.Is(err, types.ErrDocumentNotFound) {
		t.Errorf("FindByFingerprint(old) err = %v, want ErrDocumentNotFound", err)
	}

	original, err := store.GetOriginal(ctx, first.DocumentUUID)
	if err != nil {
		t.Fatalf("GetOriginal: %v", err)
	}
	if string(original) != "updated content" {
		t.Errorf("original = %q, want the updated bytes", original)
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM rag_document`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("stored %d documents, want 1", n)
	}
}
