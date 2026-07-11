package pgstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/urmzd/saige/postgres"
	"github.com/urmzd/saige/rag/types"
)

// embDim matches the default RAGEmbeddingDim used by postgres.RunMigrations
// when MigrationOptions is zero (vector(768) on rag_variant.embedding).
const embDim = 768

// testPool connects to the database named by SAIGE_TEST_POSTGRES_DSN, runs
// migrations, and truncates the rag tables so each test starts clean. Tests
// are skipped when the env var is unset (e.g. plain `go test ./...`).
// The database needs the pgvector extension available; a disposable instance:
//
//	docker run --rm -e POSTGRES_PASSWORD=test -p 5433:5432 pgvector/pgvector:pg17
//	SAIGE_TEST_POSTGRES_DSN=postgres://postgres:test@localhost:5433/postgres go test ./rag/pgstore/
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("SAIGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SAIGE_TEST_POSTGRES_DSN not set; skipping PostgreSQL integration test")
	}

	ctx := context.Background()

	// postgres.NewPool registers pgvector types at connect time, so the
	// extension must exist before it can connect; bootstrap it with a plain
	// connection first.
	boot, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("bootstrap connect: %v", err)
	}
	_, err = boot.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`)
	boot.Close()
	if err != nil {
		t.Fatalf("create vector extension: %v", err)
	}

	pool, err := postgres.NewPool(ctx, postgres.Config{URL: dsn})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := postgres.RunMigrations(ctx, pool, postgres.MigrationOptions{}); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE rag_document, rag_original, rag_section, rag_variant`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool
}

// vec builds a full-dimension embedding whose leading components are vals and
// the rest zero. Zero padding does not change cosine similarity, so tests can
// reason about angles in 2-3 dimensions.
func vec(vals ...float32) []float32 {
	v := make([]float32, embDim)
	copy(v, vals)
	return v
}

func testTime() time.Time {
	return time.Now().UTC().Truncate(time.Microsecond)
}

// singleSectionDoc builds a document with one section holding the given
// variants. Variant SectionUUIDs are filled in.
func singleSectionDoc(uuid, fingerprint string, meta map[string]string, variants ...types.ContentVariant) *types.Document {
	secUUID := uuid + "-sec-0"
	for i := range variants {
		variants[i].SectionUUID = secUUID
	}
	now := testTime()
	return &types.Document{
		UUID:        uuid,
		SourceURI:   "file:///" + uuid + ".md",
		Fingerprint: fingerprint,
		Title:       "Title of " + uuid,
		Metadata:    meta,
		Sections: []types.Section{{
			UUID:         secUUID,
			DocumentUUID: uuid,
			Index:        0,
			Heading:      "Heading 0",
			Variants:     variants,
		}},
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// TestCreateDocumentPersistsTree verifies CreateDocument writes the whole
// document tree (document + sections + variants with embeddings and metadata)
// and that every read path sees it: GetDocument, GetSections, GetVariant, and
// FindByFingerprint.
func TestCreateDocumentPersistsTree(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := NewStore(pool, nil)

	now := testTime()
	doc := &types.Document{
		UUID:        "doc-1",
		SourceURI:   "file:///doc-1.md",
		Fingerprint: "fp-doc-1",
		Title:       "Doc One",
		Metadata:    map[string]string{"team": "core", "lang": "en"},
		Sections: []types.Section{
			{
				UUID: "sec-1", DocumentUUID: "doc-1", Index: 0, Heading: "Intro",
				Variants: []types.ContentVariant{
					{
						UUID: "var-1", SectionUUID: "sec-1",
						ContentType: types.ContentText, MIMEType: "text/plain",
						Text:      "hello world",
						Embedding: vec(1, 0),
						Metadata:  map[string]string{"chunk": "0"},
					},
					{
						UUID: "var-2", SectionUUID: "sec-1",
						ContentType: types.ContentImage, MIMEType: "image/png",
						Data:      []byte{0x89, 0x50, 0x4e, 0x47},
						Embedding: vec(0, 1),
					},
				},
			},
			{
				UUID: "sec-2", DocumentUUID: "doc-1", Index: 1, Heading: "Body",
				Variants: []types.ContentVariant{
					{
						UUID: "var-3", SectionUUID: "sec-2",
						ContentType: types.ContentText, MIMEType: "text/plain",
						Text: "no embedding yet",
					},
				},
			},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := store.CreateDocument(ctx, doc); err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}

	// GetDocument returns the full tree.
	got, err := store.GetDocument(ctx, "doc-1")
	if err != nil {
		t.Fatalf("GetDocument: %v", err)
	}
	if got.SourceURI != doc.SourceURI || got.Fingerprint != doc.Fingerprint || got.Title != doc.Title {
		t.Errorf("document scalars = %q/%q/%q, want %q/%q/%q",
			got.SourceURI, got.Fingerprint, got.Title, doc.SourceURI, doc.Fingerprint, doc.Title)
	}
	if got.Metadata["team"] != "core" || got.Metadata["lang"] != "en" {
		t.Errorf("document metadata = %v, want team=core lang=en", got.Metadata)
	}
	if !got.CreatedAt.Equal(now) || !got.UpdatedAt.Equal(now) {
		t.Errorf("timestamps = %v/%v, want %v", got.CreatedAt, got.UpdatedAt, now)
	}

	// GetSections: both sections in index order, with all variants attached.
	sections, err := store.GetSections(ctx, "doc-1")
	if err != nil {
		t.Fatalf("GetSections: %v", err)
	}
	if len(sections) != 2 {
		t.Fatalf("got %d sections, want 2", len(sections))
	}
	if sections[0].UUID != "sec-1" || sections[0].Index != 0 || sections[0].Heading != "Intro" {
		t.Errorf("section 0 = %+v, want sec-1/0/Intro", sections[0])
	}
	if sections[1].UUID != "sec-2" || sections[1].Index != 1 {
		t.Errorf("section 1 = %+v, want sec-2/1", sections[1])
	}
	if len(sections[0].Variants) != 2 || len(sections[1].Variants) != 1 {
		t.Fatalf("variant counts = %d/%d, want 2/1", len(sections[0].Variants), len(sections[1].Variants))
	}
	v1 := sections[0].Variants[0]
	if v1.UUID != "var-1" || v1.Text != "hello world" || v1.ContentType != types.ContentText {
		t.Errorf("variant 1 = %+v", v1)
	}
	if len(v1.Embedding) != embDim || v1.Embedding[0] != 1 {
		t.Errorf("variant 1 embedding not round-tripped (len=%d)", len(v1.Embedding))
	}
	if v1.Metadata["chunk"] != "0" {
		t.Errorf("variant 1 metadata = %v, want chunk=0", v1.Metadata)
	}
	v2 := sections[0].Variants[1]
	if string(v2.Data) != string([]byte{0x89, 0x50, 0x4e, 0x47}) {
		t.Errorf("variant 2 binary data not round-tripped: %v", v2.Data)
	}
	if sections[1].Variants[0].Embedding != nil {
		t.Errorf("variant 3 should have nil embedding, got len %d", len(sections[1].Variants[0].Embedding))
	}

	// GetVariant returns the variant plus full provenance.
	v, prov, err := store.GetVariant(ctx, "var-1")
	if err != nil {
		t.Fatalf("GetVariant: %v", err)
	}
	if v.UUID != "var-1" || v.SectionUUID != "sec-1" || v.Metadata["chunk"] != "0" {
		t.Errorf("GetVariant variant = %+v", v)
	}
	if prov.DocumentUUID != "doc-1" || prov.DocumentTitle != "Doc One" ||
		prov.SourceURI != doc.SourceURI || prov.SectionUUID != "sec-1" ||
		prov.SectionHeading != "Intro" || prov.SectionIndex != 0 {
		t.Errorf("GetVariant provenance = %+v", prov)
	}

	// FindByFingerprint resolves to the same full document.
	byFP, err := store.FindByFingerprint(ctx, "fp-doc-1")
	if err != nil {
		t.Fatalf("FindByFingerprint: %v", err)
	}
	if byFP.UUID != "doc-1" || len(byFP.Sections) != 2 {
		t.Errorf("FindByFingerprint = %s with %d sections, want doc-1 with 2", byFP.UUID, len(byFP.Sections))
	}
}

// TestReplaceDocumentSwapsAtomically replaces an existing document and checks
// the old tree is fully gone, the new tree is fully present, and the
// fingerprint index tracks the swap. The same-fingerprint subtest exercises
// the delete-before-insert ordering: re-ingesting content with an unchanged
// fingerprint must not trip the unique fingerprint index.
func TestReplaceDocumentSwapsAtomically(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := NewStore(pool, nil)

	old := singleSectionDoc("doc-old", "fp-old", map[string]string{"v": "1"},
		types.ContentVariant{
			UUID: "var-old", ContentType: types.ContentText, MIMEType: "text/plain",
			Text: "old text", Embedding: vec(1, 0),
		})
	if err := store.CreateDocument(ctx, old); err != nil {
		t.Fatalf("CreateDocument(old): %v", err)
	}

	newDoc := singleSectionDoc("doc-new", "fp-new", map[string]string{"v": "2"},
		types.ContentVariant{
			UUID: "var-new", ContentType: types.ContentText, MIMEType: "text/plain",
			Text: "new text", Embedding: vec(0, 1),
			Metadata: map[string]string{"chunk": "0"},
		})
	if err := store.ReplaceDocument(ctx, "doc-old", newDoc); err != nil {
		t.Fatalf("ReplaceDocument: %v", err)
	}

	// Old tree is gone from every read path.
	if _, err := store.GetDocument(ctx, "doc-old"); !errors.Is(err, types.ErrDocumentNotFound) {
		t.Errorf("GetDocument(doc-old) err = %v, want ErrDocumentNotFound", err)
	}
	if _, _, err := store.GetVariant(ctx, "var-old"); !errors.Is(err, types.ErrVariantNotFound) {
		t.Errorf("GetVariant(var-old) err = %v, want ErrVariantNotFound", err)
	}
	if _, err := store.FindByFingerprint(ctx, "fp-old"); !errors.Is(err, types.ErrDocumentNotFound) {
		t.Errorf("FindByFingerprint(fp-old) err = %v, want ErrDocumentNotFound", err)
	}

	// New tree is fully present, including via the fingerprint index.
	got, err := store.FindByFingerprint(ctx, "fp-new")
	if err != nil {
		t.Fatalf("FindByFingerprint(fp-new): %v", err)
	}
	if got.UUID != "doc-new" || len(got.Sections) != 1 || len(got.Sections[0].Variants) != 1 {
		t.Fatalf("replacement tree incomplete: %+v", got)
	}
	if got.Sections[0].Variants[0].Text != "new text" {
		t.Errorf("replacement variant text = %q, want %q", got.Sections[0].Variants[0].Text, "new text")
	}

	t.Run("same fingerprint does not trip unique index", func(t *testing.T) {
		again := singleSectionDoc("doc-new-2", "fp-new", nil,
			types.ContentVariant{
				UUID: "var-new-2", ContentType: types.ContentText, MIMEType: "text/plain",
				Text: "re-ingested", Embedding: vec(0, 1),
			})
		if err := store.ReplaceDocument(ctx, "doc-new", again); err != nil {
			t.Fatalf("ReplaceDocument with same fingerprint: %v", err)
		}
		got, err := store.FindByFingerprint(ctx, "fp-new")
		if err != nil {
			t.Fatalf("FindByFingerprint after same-fp replace: %v", err)
		}
		if got.UUID != "doc-new-2" {
			t.Errorf("fingerprint fp-new resolves to %s, want doc-new-2", got.UUID)
		}
	})
}

// TestReplaceDocumentFailureKeepsOldDocument forces a mid-transaction failure
// (a variant whose embedding has the wrong dimension, rejected by the
// vector(768) column) and verifies ReplaceDocument errors while the prior
// document survives completely intact: document row, sections, variants,
// embeddings, and fingerprint lookup.
func TestReplaceDocumentFailureKeepsOldDocument(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := NewStore(pool, nil)

	old := singleSectionDoc("doc-keep", "fp-keep", map[string]string{"v": "1"},
		types.ContentVariant{
			UUID: "var-keep-1", ContentType: types.ContentText, MIMEType: "text/plain",
			Text: "keep me", Embedding: vec(1, 0),
			Metadata: map[string]string{"chunk": "0"},
		},
		types.ContentVariant{
			UUID: "var-keep-2", ContentType: types.ContentText, MIMEType: "text/plain",
			Text: "keep me too", Embedding: vec(0, 1),
		})
	if err := store.CreateDocument(ctx, old); err != nil {
		t.Fatalf("CreateDocument(old): %v", err)
	}

	// The new document's second variant carries a 3-dim embedding, which the
	// vector(768) column rejects — after the document row and first variant
	// were already written inside the transaction.
	bad := singleSectionDoc("doc-bad", "fp-bad", nil,
		types.ContentVariant{
			UUID: "var-bad-ok", ContentType: types.ContentText, MIMEType: "text/plain",
			Text: "fine", Embedding: vec(1, 1),
		},
		types.ContentVariant{
			UUID: "var-bad-dim", ContentType: types.ContentText, MIMEType: "text/plain",
			Text: "wrong dimension", Embedding: []float32{1, 2, 3},
		})
	if err := store.ReplaceDocument(ctx, "doc-keep", bad); err == nil {
		t.Fatal("ReplaceDocument with wrong-dimension embedding succeeded, want error")
	}

	// The prior document is fully retrievable.
	got, err := store.GetDocument(ctx, "doc-keep")
	if err != nil {
		t.Fatalf("GetDocument(doc-keep) after failed replace: %v", err)
	}
	if len(got.Sections) != 1 || len(got.Sections[0].Variants) != 2 {
		t.Fatalf("prior tree damaged: %d sections / %d variants, want 1/2",
			len(got.Sections), len(got.Sections[0].Variants))
	}
	if got.Metadata["v"] != "1" {
		t.Errorf("prior metadata = %v, want v=1", got.Metadata)
	}
	v, prov, err := store.GetVariant(ctx, "var-keep-1")
	if err != nil {
		t.Fatalf("GetVariant(var-keep-1): %v", err)
	}
	if v.Text != "keep me" || len(v.Embedding) != embDim || prov.DocumentUUID != "doc-keep" {
		t.Errorf("prior variant damaged: %+v / %+v", v, prov)
	}
	if got, err := store.FindByFingerprint(ctx, "fp-keep"); err != nil || got.UUID != "doc-keep" {
		t.Errorf("FindByFingerprint(fp-keep) = %v, %v; want doc-keep", got, err)
	}

	// Nothing from the failed replacement leaked in.
	if _, err := store.GetDocument(ctx, "doc-bad"); !errors.Is(err, types.ErrDocumentNotFound) {
		t.Errorf("GetDocument(doc-bad) err = %v, want ErrDocumentNotFound", err)
	}
	if _, _, err := store.GetVariant(ctx, "var-bad-ok"); !errors.Is(err, types.ErrVariantNotFound) {
		t.Errorf("GetVariant(var-bad-ok) err = %v, want ErrVariantNotFound", err)
	}
}

// TestSearchMetadataFilterPushdownDeepRows is the regression test for the
// old limit*3 over-fetch heuristic: 40 variants where the 5 qualifying rows
// rank 36th-40th by similarity, far past 3x the limit of 3. With the filter
// pushed into SQL, the search must still return 3 qualifying rows in
// similarity order.
func TestSearchMetadataFilterPushdownDeepRows(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := NewStore(pool, nil)

	// Query axis is e0. Non-matching variants sit at tiny angles from e0
	// (cos > 0.94); matching ones sit much further out (cos < 0.45), so all
	// 35 non-matching rows rank above every matching row.
	variants := make([]types.ContentVariant, 0, 40)
	for i := 0; i < 35; i++ {
		variants = append(variants, types.ContentVariant{
			UUID:        fmt.Sprintf("var-noise-%02d", i),
			ContentType: types.ContentText, MIMEType: "text/plain",
			Text:      fmt.Sprintf("noise %d", i),
			Embedding: vec(1, float32(i)*0.01),
			Metadata:  map[string]string{"lang": "rust"},
		})
	}
	for j := 0; j < 5; j++ {
		variants = append(variants, types.ContentVariant{
			UUID:        fmt.Sprintf("var-match-%d", j),
			ContentType: types.ContentText, MIMEType: "text/plain",
			Text:      fmt.Sprintf("match %d", j),
			Embedding: vec(1, 2.0+0.2*float32(j)),
			Metadata:  map[string]string{"lang": "go"},
		})
	}
	doc := singleSectionDoc("doc-deep", "fp-deep", nil, variants...)
	if err := store.CreateDocument(ctx, doc); err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}

	hits, err := store.SearchByEmbedding(ctx, vec(1, 0), &types.SearchOptions{
		Limit:           3,
		MetadataFilters: []types.MetadataFilter{{Key: "lang", Op: types.FilterEq, Value: "go"}},
	})
	if err != nil {
		t.Fatalf("SearchByEmbedding: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("got %d hits, want 3 (old limit*3 heuristic would have found 0)", len(hits))
	}
	// The nearest matching variants, in similarity order.
	want := []string{"var-match-0", "var-match-1", "var-match-2"}
	for i, hit := range hits {
		if hit.Variant.UUID != want[i] {
			t.Errorf("hit %d = %s (score %.4f), want %s", i, hit.Variant.UUID, hit.Score, want[i])
		}
		if hit.Variant.Metadata["lang"] != "go" {
			t.Errorf("hit %d lang = %q, want go", i, hit.Variant.Metadata["lang"])
		}
	}
}

// TestSearchFilterNeqAndContains verifies neq and contains semantics against
// real JSONB: neq passes rows where the key is absent, contains requires the
// key to exist and match as a substring.
func TestSearchFilterNeqAndContains(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := NewStore(pool, nil)

	doc := singleSectionDoc("doc-flt", "fp-flt", nil,
		types.ContentVariant{
			UUID: "var-draft", ContentType: types.ContentText, MIMEType: "text/plain",
			Text: "draft", Embedding: vec(1, 0),
			Metadata: map[string]string{"draft": "true", "tags": "golang,database"},
		},
		types.ContentVariant{
			UUID: "var-final", ContentType: types.ContentText, MIMEType: "text/plain",
			Text: "final", Embedding: vec(1, 0.1),
			Metadata: map[string]string{"draft": "false", "tags": "frontend"},
		},
		types.ContentVariant{
			UUID: "var-bare", ContentType: types.ContentText, MIMEType: "text/plain",
			Text: "bare", Embedding: vec(1, 0.2),
		})
	if err := store.CreateDocument(ctx, doc); err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}

	search := func(t *testing.T, f types.MetadataFilter) map[string]bool {
		t.Helper()
		hits, err := store.SearchByEmbedding(ctx, vec(1, 0), &types.SearchOptions{
			Limit:           10,
			MetadataFilters: []types.MetadataFilter{f},
		})
		if err != nil {
			t.Fatalf("SearchByEmbedding(%+v): %v", f, err)
		}
		got := make(map[string]bool, len(hits))
		for _, h := range hits {
			got[h.Variant.UUID] = true
		}
		return got
	}

	t.Run("neq excludes matching value but passes absent key", func(t *testing.T) {
		got := search(t, types.MetadataFilter{Key: "draft", Op: types.FilterNeq, Value: "true"})
		if got["var-draft"] {
			t.Error("neq draft=true returned var-draft")
		}
		if !got["var-final"] {
			t.Error("neq draft=true dropped var-final (draft=false)")
		}
		if !got["var-bare"] {
			t.Error("neq draft=true dropped var-bare: absent key must pass neq (memstore parity)")
		}
	})

	t.Run("contains matches substring and requires key present", func(t *testing.T) {
		got := search(t, types.MetadataFilter{Key: "tags", Op: types.FilterContains, Value: "go"})
		if !got["var-draft"] {
			t.Error("contains tags~go dropped var-draft (tags=golang,database)")
		}
		if got["var-final"] {
			t.Error("contains tags~go returned var-final (tags=frontend)")
		}
		if got["var-bare"] {
			t.Error("contains tags~go returned var-bare: absent key must fail contains")
		}
	})
}

// TestSearchVariantMetadataOverridesDocument checks the merged-metadata
// semantics match memstore's mergeMetadata: the variant's value wins for a
// shared key, and document-level keys apply to variants that do not override
// them.
func TestSearchVariantMetadataOverridesDocument(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := NewStore(pool, nil)

	doc := singleSectionDoc("doc-merge", "fp-merge",
		map[string]string{"lang": "python", "team": "core"},
		types.ContentVariant{
			UUID: "var-override", ContentType: types.ContentText, MIMEType: "text/plain",
			Text: "overrides lang", Embedding: vec(1, 0),
			Metadata: map[string]string{"lang": "go"},
		},
		types.ContentVariant{
			UUID: "var-inherit", ContentType: types.ContentText, MIMEType: "text/plain",
			Text: "inherits lang", Embedding: vec(1, 0.1),
		})
	if err := store.CreateDocument(ctx, doc); err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}

	search := func(t *testing.T, key, value string) map[string]bool {
		t.Helper()
		hits, err := store.SearchByEmbedding(ctx, vec(1, 0), &types.SearchOptions{
			Limit:           10,
			MetadataFilters: []types.MetadataFilter{{Key: key, Op: types.FilterEq, Value: value}},
		})
		if err != nil {
			t.Fatalf("SearchByEmbedding(%s=%s): %v", key, value, err)
		}
		got := make(map[string]bool, len(hits))
		for _, h := range hits {
			got[h.Variant.UUID] = true
		}
		return got
	}

	if got := search(t, "lang", "go"); !got["var-override"] || got["var-inherit"] {
		t.Errorf("lang=go hits = %v, want only var-override (variant value wins)", got)
	}
	if got := search(t, "lang", "python"); got["var-override"] || !got["var-inherit"] {
		t.Errorf("lang=python hits = %v, want only var-inherit (doc value shadowed on var-override)", got)
	}
	if got := search(t, "team", "core"); !got["var-override"] || !got["var-inherit"] {
		t.Errorf("team=core hits = %v, want both (doc-level key inherited)", got)
	}
}

// TestSearchOrderingAndMinScore verifies the nearest embedding ranks first
// with descending cosine-similarity scores, and that MinScore drops
// below-threshold rows.
func TestSearchOrderingAndMinScore(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := NewStore(pool, nil)

	// cos to query e0: var-near = 1.0, var-mid ~ 0.707, var-far ~ 0.196.
	doc := singleSectionDoc("doc-ord", "fp-ord", nil,
		types.ContentVariant{
			UUID: "var-far", ContentType: types.ContentText, MIMEType: "text/plain",
			Text: "far", Embedding: vec(1, 5),
		},
		types.ContentVariant{
			UUID: "var-near", ContentType: types.ContentText, MIMEType: "text/plain",
			Text: "near", Embedding: vec(1, 0),
		},
		types.ContentVariant{
			UUID: "var-mid", ContentType: types.ContentText, MIMEType: "text/plain",
			Text: "mid", Embedding: vec(1, 1),
		})
	if err := store.CreateDocument(ctx, doc); err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}

	hits, err := store.SearchByEmbedding(ctx, vec(1, 0), &types.SearchOptions{Limit: 10})
	if err != nil {
		t.Fatalf("SearchByEmbedding: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("got %d hits, want 3", len(hits))
	}
	wantOrder := []string{"var-near", "var-mid", "var-far"}
	for i, hit := range hits {
		if hit.Variant.UUID != wantOrder[i] {
			t.Errorf("hit %d = %s, want %s", i, hit.Variant.UUID, wantOrder[i])
		}
	}
	if !(hits[0].Score >= hits[1].Score && hits[1].Score >= hits[2].Score) {
		t.Errorf("scores not descending: %.4f, %.4f, %.4f", hits[0].Score, hits[1].Score, hits[2].Score)
	}
	if hits[0].Score < 0.999 {
		t.Errorf("identical vector scored %.4f, want ~1.0", hits[0].Score)
	}
	// Provenance and timestamp are populated by the search projection.
	if hits[0].Provenance.DocumentUUID != "doc-ord" || hits[0].Provenance.SectionUUID != "doc-ord-sec-0" {
		t.Errorf("hit provenance = %+v", hits[0].Provenance)
	}
	if hits[0].Timestamp.IsZero() {
		t.Error("hit timestamp is zero, want document updated_at")
	}

	// MinScore 0.5 keeps var-near (1.0) and var-mid (~0.707), drops var-far.
	hits, err = store.SearchByEmbedding(ctx, vec(1, 0), &types.SearchOptions{Limit: 10, MinScore: 0.5})
	if err != nil {
		t.Fatalf("SearchByEmbedding with MinScore: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("MinScore=0.5: got %d hits, want 2", len(hits))
	}
	for _, hit := range hits {
		if hit.Score < 0.5 {
			t.Errorf("hit %s score %.4f below MinScore", hit.Variant.UUID, hit.Score)
		}
		if hit.Variant.UUID == "var-far" {
			t.Error("var-far returned despite MinScore=0.5")
		}
	}
}
