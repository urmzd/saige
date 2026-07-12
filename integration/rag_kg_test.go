package integration

import (
	"context"
	"errors"
	"hash/fnv"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/urmzd/saige/knowledge"
	kgpgstore "github.com/urmzd/saige/knowledge/pgstore"
	knowledgetypes "github.com/urmzd/saige/knowledge/types"
	"github.com/urmzd/saige/rag"
	"github.com/urmzd/saige/rag/embedderregistry"
	"github.com/urmzd/saige/rag/extractor"
	ragpgstore "github.com/urmzd/saige/rag/pgstore"
	ragtypes "github.com/urmzd/saige/rag/types"
)

// kgFakeExtractor is a deterministic knowledge extractor so no LLM is needed.
// It returns the same entities/relation for every episode, which also
// exercises the engine's exact-match entity dedup and edge dedup across the
// document's sections.
type kgFakeExtractor struct{}

func (e *kgFakeExtractor) Extract(_ context.Context, _ string) ([]knowledgetypes.ExtractedEntity, []knowledgetypes.ExtractedRelation, error) {
	return []knowledgetypes.ExtractedEntity{
			{Name: "Ada Lovelace", Type: "Person", Summary: "pioneering programmer of the Analytical Engine"},
			{Name: "Analytical Engine", Type: "Machine", Summary: "mechanical general-purpose computer designed by Babbage"},
		}, []knowledgetypes.ExtractedRelation{
			{Source: "Ada Lovelace", Target: "Analytical Engine", Type: "WROTE_PROGRAMS_FOR",
				Fact: "Ada Lovelace wrote programs for the Analytical Engine"},
		}, nil
}

// hashVariantEmbedder is a deterministic 768-dim bag-of-words embedder that
// matches the vector(768) columns created by postgres.RunMigrations defaults.
type hashVariantEmbedder struct{}

func (e *hashVariantEmbedder) Embed(_ context.Context, variants []ragtypes.ContentVariant) ([][]float32, error) {
	out := make([][]float32, len(variants))
	for i, v := range variants {
		out[i] = hashEmbed(v.Text, migratedEmbedDim)
	}
	return out, nil
}

// hashEmbed maps each lowercase token into a dimension bucket via FNV-1a and
// L2-normalizes the result, so overlapping vocabulary yields cosine similarity.
func hashEmbed(text string, dim int) []float32 {
	vec := make([]float32, dim)
	for _, tok := range strings.Fields(strings.ToLower(text)) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(tok))
		vec[int(h.Sum32())%dim]++
	}
	var norm float64
	for _, x := range vec {
		norm += float64(x) * float64(x)
	}
	if norm == 0 {
		vec[0] = 1
		return vec
	}
	inv := float32(1 / math.Sqrt(norm))
	for i := range vec {
		vec[i] *= inv
	}
	return vec
}

// countRows returns the result of a single-integer COUNT query.
func countRows(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

// TestRAGKnowledgeGraphRoundTrip is the cross-package end-to-end test for the
// RAG + KG integration over a real PostgreSQL on both sides:
//
//   - rag.NewPipeline(WithStore(rag/pgstore), WithGraph(knowledge graph over
//     knowledge/pgstore)) ingests a document, producing kg_episode rows
//     grouped by the document UUID plus entities and relations in the group;
//   - Search returns hits through the registered graph retriever (proved via
//     a graph-only pipeline that has no other retriever);
//   - Delete removes the document AND every derived kg row for its group, so
//     graph facts never outlive their source document.
func TestRAGKnowledgeGraphRoundTrip(t *testing.T) {
	pool := requirePostgres(t)
	truncate(t, pool, "rag_document", "kg_entity", "kg_episode")
	ctx := testContext(t, 2*time.Minute)

	// Knowledge side: public constructor with an injected fake extractor and
	// no embedder: SearchFacts degrades to fulltext-only, no LLM required.
	kgStore := kgpgstore.NewStore(pool, nil)
	graph, err := knowledge.NewGraph(ctx,
		knowledge.WithStore(kgStore),
		knowledge.WithExtractor(&kgFakeExtractor{}),
	)
	if err != nil {
		t.Fatalf("new graph: %v", err)
	}

	// RAG side: real pg store, real plaintext content extractor, fake embedder.
	ragStore := ragpgstore.NewStore(pool, nil)
	pipe, err := rag.NewPipeline(
		rag.WithStore(ragStore),
		rag.WithContentExtractor(extractor.NewAuto()),
		rag.WithEmbedders(embedderregistry.NewTextOnly(&hashVariantEmbedder{})),
		rag.WithGraph(graph),
	)
	if err != nil {
		t.Fatalf("new pipeline: %v", err)
	}

	// ── Ingest ────────────────────────────────────────────────────────
	res, err := pipe.Ingest(ctx, &ragtypes.RawDocument{
		SourceURI: "test://ada-lovelace",
		MIMEType:  "text/plain",
		Data: []byte("Ada Lovelace wrote programs for the Analytical Engine.\n\n" +
			"The Analytical Engine was designed by Charles Babbage."),
	})
	if err != nil {
		t.Fatalf("ingest (want fully clean, not even ErrPartialIngest): %v", err)
	}
	docUUID := res.DocumentUUID
	if docUUID == "" {
		t.Fatal("ingest returned empty document UUID")
	}
	if res.Sections != 2 {
		t.Fatalf("ingest sections = %d, want 2", res.Sections)
	}

	// kg_episode rows exist, grouped by the document UUID (one per text variant).
	if n := countRows(t, pool, `SELECT count(*) FROM kg_episode WHERE group_id = $1`, docUUID); n != 2 {
		t.Errorf("kg_episode rows for group %s = %d, want 2", docUUID, n)
	}
	// Entities dedup within the group: two distinct entities despite two episodes.
	if n := countRows(t, pool, `SELECT count(*) FROM kg_entity WHERE group_id = $1`, docUUID); n != 2 {
		t.Errorf("kg_entity rows for group %s = %d, want 2", docUUID, n)
	}
	// Edge dedup: the identical relation from the second episode is skipped.
	if n := countRows(t, pool, `SELECT count(*) FROM kg_relation WHERE group_id = $1`, docUUID); n != 1 {
		t.Errorf("kg_relation rows for group %s = %d, want 1", docUUID, n)
	}

	// ── Search ────────────────────────────────────────────────────────
	// The KG itself answers over Postgres fulltext.
	facts, err := graph.SearchFacts(ctx, "Ada Lovelace")
	if err != nil && !errors.Is(err, knowledgetypes.ErrPartialSearch) {
		t.Fatalf("search facts: %v", err)
	}
	if len(facts.Facts) == 0 {
		t.Fatal("SearchFacts returned no facts for ingested entities")
	}
	if got := facts.Facts[0].FactText; !strings.Contains(got, "Analytical Engine") {
		t.Errorf("fact text = %q, want mention of Analytical Engine", got)
	}

	// The full pipeline (vector + graph retrievers fused) returns hits.
	searchRes, err := pipe.Search(ctx, "Ada Lovelace")
	if err != nil {
		t.Fatalf("pipeline search: %v", err)
	}
	if len(searchRes.Hits) == 0 {
		t.Fatal("pipeline search returned no hits")
	}

	// Isolate the graph retriever: a pipeline whose ONLY retriever is the one
	// WithGraph registers. Hits here can only have come through the KG, and
	// provenance must resolve back to the ingested document via episode GroupID.
	graphOnly, err := rag.NewPipeline(
		rag.WithStore(ragStore),
		rag.WithContentExtractor(extractor.NewAuto()),
		rag.WithGraph(graph),
	)
	if err != nil {
		t.Fatalf("new graph-only pipeline: %v", err)
	}
	graphRes, err := graphOnly.Search(ctx, "Ada Lovelace")
	if err != nil {
		t.Fatalf("graph-only search: %v", err)
	}
	if len(graphRes.Hits) == 0 {
		t.Fatal("graph retriever returned no hits for ingested document")
	}
	foundProvenance := false
	for _, hit := range graphRes.Hits {
		if hit.Provenance.DocumentUUID == docUUID {
			foundProvenance = true
			// Both ingested paragraphs mention the Analytical Engine; a resolved
			// hit must carry one of them, not synthetic fact text.
			if !strings.Contains(hit.Variant.Text, "Analytical Engine") {
				t.Errorf("graph hit resolved to variant %q, want an ingested paragraph", hit.Variant.Text)
			}
		}
	}
	if !foundProvenance {
		t.Errorf("no graph hit resolved provenance to document %s; hits: %+v", docUUID, graphRes.Hits)
	}

	// ── Delete ────────────────────────────────────────────────────────
	if err := pipe.Delete(ctx, docUUID); err != nil {
		t.Fatalf("delete document: %v", err)
	}

	for _, tbl := range []string{"kg_episode", "kg_relation", "kg_entity"} {
		if n := countRows(t, pool, `SELECT count(*) FROM `+tbl+` WHERE group_id = $1`, docUUID); n != 0 {
			t.Errorf("%s rows for group %s after delete = %d, want 0", tbl, docUUID, n)
		}
	}
	if n := countRows(t, pool, `SELECT count(*) FROM kg_mention`); n != 0 {
		t.Errorf("kg_mention rows after delete = %d, want 0", n)
	}
	if _, err := ragStore.GetDocument(ctx, docUUID); !errors.Is(err, ragtypes.ErrDocumentNotFound) {
		t.Errorf("GetDocument after delete: err = %v, want ErrDocumentNotFound", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM rag_document`); n != 0 {
		t.Errorf("rag_document rows after delete = %d, want 0", n)
	}
}
