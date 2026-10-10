// RAG on Postgres: ingest a document, then run a hybrid search that fuses
// pgvector similarity with pg_search BM25. Embeddings come from a local
// Ollama runtime serving nomic-embed-text (768 dimensions, the default
// column size).
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/urmzd/saige/agent/provider/ollama"
	"github.com/urmzd/saige/postgres"
	"github.com/urmzd/saige/rag"
	"github.com/urmzd/saige/rag/embedderregistry"
	"github.com/urmzd/saige/rag/extractor"
	"github.com/urmzd/saige/rag/pgstore"
	ragtypes "github.com/urmzd/saige/rag/types"
)

const dsn = "postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"

func main() {
	ctx := context.Background()
	pool, err := postgres.NewPool(ctx, postgres.Config{URL: dsn})
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	if err := postgres.RunMigrations(ctx, pool, postgres.MigrationOptions{}); err != nil {
		log.Fatal(err)
	}

	client, err := ollama.NewClient(ollama.Config{Host: "http://localhost:11434", EmbeddingModel: "nomic-embed-text"})
	if err != nil {
		log.Fatal(err)
	}
	store, err := pgstore.New(pgstore.Config{Pool: pool})
	if err != nil {
		log.Fatal(err)
	}
	emb := ollama.NewEmbedder(client)
	pipe, err := rag.New(rag.Config{Store: store},
		rag.WithContentExtractor(extractor.NewAuto()),
		rag.WithEmbedders(embedderregistry.NewTextOnly(embedderregistry.Text(emb))),
		rag.WithRecursiveChunker(512, 64),
		rag.WithBM25(nil), // keyword search runs in Postgres through pg_search
	)
	if err != nil {
		log.Fatal(err)
	}
	defer pipe.Close(ctx)

	doc := "saige stores vectors, BM25 indexes and conversation trees in one Postgres 18 database."
	if _, err := pipe.Ingest(ctx, &ragtypes.RawDocument{SourceURI: "notes://saige", MIMEType: "text/plain", Data: []byte(doc)}); err != nil {
		log.Fatal(err)
	}
	res, err := pipe.Search(ctx, "Which database does saige use?", ragtypes.WithLimit(3))
	if err != nil {
		log.Fatal(err)
	}
	for _, hit := range res.Hits {
		fmt.Printf("%.3f %s\n", hit.Score, hit.Variant.Text)
	}
}
