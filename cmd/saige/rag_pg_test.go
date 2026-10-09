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

// TestRAGSearchFindsDocumentsIngestedByAnotherPipeline mirrors running
// `saige rag ingest` and `saige rag search` as two separate processes: the
// search pipeline is built fresh and must still find the ingested document.
func TestRAGSearchFindsDocumentsIngestedByAnotherPipeline(t *testing.T) {
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
