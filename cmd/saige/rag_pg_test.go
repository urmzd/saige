package main

import (
	"context"
	"hash/fnv"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/urmzd/saige/rag"
	"github.com/urmzd/saige/rag/pgstore"
	ragtypes "github.com/urmzd/saige/rag/types"
)

// hashEmbedder is a deterministic bag-of-words embedder sized for the default
// 768-dimension rag_variant.embedding column.
func hashEmbedder() *textEmbedder {
	return &textEmbedder{embed: func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i, text := range texts {
			v := make([]float32, 768)
			for _, w := range strings.Fields(strings.ToLower(text)) {
				h := fnv.New32a()
				_, _ = h.Write([]byte(strings.Trim(w, ".,;:!?")))
				v[h.Sum32()%768]++
			}
			var norm float64
			for _, x := range v {
				norm += float64(x * x)
			}
			if norm > 0 {
				for j := range v {
					v[j] /= float32(math.Sqrt(norm))
				}
			}
			out[i] = v
		}
		return out, nil
	}}
}

// ragTestPool connects to SAIGE_TEST_POSTGRES_DSN the way the CLI does and
// empties the rag tables. It skips the test when the variable is unset.
func ragTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("SAIGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SAIGE_TEST_POSTGRES_DSN not set; skipping PostgreSQL test")
	}
	ctx := context.Background()

	boot, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("bootstrap connect: %v", err)
	}
	_, err = boot.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`)
	boot.Close()
	if err != nil {
		t.Fatalf("create vector extension: %v", err)
	}
	pool, err := connectPostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE rag_document, rag_original, rag_section, rag_variant`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool
}

// TestRAGKeywordSearchSurvivesProcessBoundary mirrors `saige rag ingest`
// followed by `saige rag search` in a new process: the BM25 arm of the fresh
// pipeline must find the document, because its index lives in Postgres
// rather than in the ingesting process's memory.
func TestRAGKeywordSearchSurvivesProcessBoundary(t *testing.T) {
	pool := ragTestPool(t)
	ctx := context.Background()

	newPipeline := func() ragtypes.Pipeline {
		p, err := rag.NewPipeline(ragPipelineOptions(pgstore.NewStore(pool, nil), hashEmbedder())...)
		if err != nil {
			t.Fatalf("pipeline: %v", err)
		}
		return p
	}

	ingest := newPipeline()
	for uri, text := range map[string]string{
		"file://okapi.txt": "The okapi is a forest giraffe native to the Congo.",
		"file://kiwi.txt":  "The kiwi is a flightless bird native to New Zealand.",
	} {
		if _, err := ingest.Ingest(ctx, &ragtypes.RawDocument{SourceURI: uri, MIMEType: "text/plain", Data: []byte(text)}); err != nil {
			t.Fatalf("ingest %s: %v", uri, err)
		}
	}
	_ = ingest.Close(ctx)

	search := newPipeline()
	defer func() { _ = search.Close(ctx) }()
	res, err := search.Search(ctx, "okapi", ragtypes.WithLimit(5))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	var bm25 *ragtypes.RetrievalStat
	for i := range res.Retrievals {
		if res.Retrievals[i].Retriever == "bm25" {
			bm25 = &res.Retrievals[i]
		}
	}
	if bm25 == nil {
		t.Fatalf("no bm25 retrieval in %+v", res.Retrievals)
	}
	if bm25.Error != "" || bm25.Hits != 1 {
		t.Fatalf("bm25 retrieval = %+v, want exactly the okapi document", *bm25)
	}
	if len(res.Hits) == 0 || res.Hits[0].Provenance.SourceURI != "file://okapi.txt" {
		t.Fatalf("top hit = %+v, want file://okapi.txt", res.Hits)
	}
}

// TestRAGSearchFindsDocumentsIngestedByAnotherPipeline mirrors running
// `saige rag ingest` and `saige rag search` as two separate processes: the
// search pipeline is built fresh and must still find the ingested document.
func TestRAGSearchFindsDocumentsIngestedByAnotherPipeline(t *testing.T) {
	pool := ragTestPool(t)
	ctx := context.Background()

	newPipeline := func() ragtypes.Pipeline {
		p, err := rag.NewPipeline(ragPipelineOptions(pgstore.NewStore(pool, nil), hashEmbedder())...)
		if err != nil {
			t.Fatalf("pipeline: %v", err)
		}
		return p
	}

	ingest := newPipeline()
	if _, err := ingest.Ingest(ctx, &ragtypes.RawDocument{
		SourceURI: "file://attention.txt",
		MIMEType:  "text/plain",
		Data:      []byte("The attention mechanism lets transformers weigh every token against every other token."),
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	_ = ingest.Close(ctx)

	var embedded int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM rag_variant WHERE embedding IS NOT NULL`).Scan(&embedded); err != nil {
		t.Fatalf("count embeddings: %v", err)
	}
	if embedded == 0 {
		t.Fatal("ingest stored no embeddings")
	}

	search := newPipeline()
	defer func() { _ = search.Close(ctx) }()
	res, err := search.Search(ctx, "attention mechanism", ragtypes.WithLimit(5))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatal("search in a fresh pipeline found nothing")
	}
}
