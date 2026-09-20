// Command basic demonstrates building a knowledge graph with knowledge.
//
// Prerequisites:
//   - PostgreSQL running with pgvector extension (e.g. docker run -p 5432:5432 -e POSTGRES_PASSWORD=postgres pgvector/pgvector:pg17)
//   - Ollama running with a model pulled (e.g. ollama pull llama3.2)
//
// Usage:
//
//	go run ./examples/knowledge/basic
package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"

	"github.com/urmzd/saige/agent/provider/ollama"
	"github.com/urmzd/saige/postgres"
	"github.com/urmzd/saige/rag/knowledge"
	"github.com/urmzd/saige/rag/knowledge/types"
)

func main() {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// 1. Create shared PostgreSQL connection pool.
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"
	}
	pool, err := postgres.NewPool(ctx, postgres.Config{URL: dbURL})
	if err != nil {
		log.Fatalf("connect postgres: %v", err)
	}
	defer pool.Close()

	// Run migrations to create tables and indexes.
	if err := postgres.RunMigrations(ctx, pool, postgres.MigrationOptions{}); err != nil {
		log.Fatalf("migrations: %v", err)
	}

	// 2. Create Ollama client for LLM extraction and embedding.
	ollamaClient := ollama.NewClient("http://localhost:11434", "gemma3", "nomic-embed-text")

	// 3. Create the knowledge graph.
	graph, err := knowledge.NewGraph(ctx,
		knowledge.WithPostgres(pool),
		knowledge.WithExtractor(knowledge.NewOllamaExtractor(ollamaClient)),
		knowledge.WithEmbedder(knowledge.NewOllamaEmbedder(ollamaClient)),
		knowledge.WithLogger(logger),
	)
	if err != nil {
		log.Fatalf("create graph: %v", err)
	}
	defer graph.Close(ctx)

	// 4. (Optional) Apply an ontology to guide extraction.
	err = graph.ApplyOntology(ctx, &types.Ontology{
		EntityTypes: []types.EntityTypeDef{
			{Name: "Person", Description: "A human being"},
			{Name: "Organization", Description: "A company or institution"},
			{Name: "Technology", Description: "A programming language, framework, or tool"},
		},
		RelationTypes: []types.RelationTypeDef{
			{Name: "works_at", Description: "Employment relationship", SourceType: "Person", TargetType: "Organization"},
			{Name: "uses", Description: "Uses a technology", SourceType: "Person", TargetType: "Technology"},
			{Name: "develops", Description: "Develops or maintains", SourceType: "Organization", TargetType: "Technology"},
		},
	})
	if err != nil {
		log.Fatalf("apply ontology: %v", err)
	}

	// 5. Ingest episodes of text.
	episodes := []types.EpisodeInput{
		{
			Name:    "team-intro",
			Body:    "Alice is a software engineer at Acme Corp. She primarily uses Go and Python for backend services.",
			Source:  "onboarding doc",
			GroupID: "engineering",
		},
		{
			Name:    "project-update",
			Body:    "Bob joined Acme Corp last month. He works with Alice on the data pipeline team and is learning Go.",
			Source:  "standup notes",
			GroupID: "engineering",
		},
	}

	for _, ep := range episodes {
		result, err := graph.IngestEpisode(ctx, &ep)
		if err != nil {
			log.Printf("ingest %q: %v", ep.Name, err)
			continue
		}
		fmt.Printf("Ingested %q: %d entities, %d relations\n",
			result.Name, len(result.EntityNodes), len(result.EpisodicEdges))
	}

	// 6. Search for facts.
	searchResult, err := graph.SearchFacts(ctx, "Who works at Acme?",
		types.WithLimit(5),
		types.WithGroupID("engineering"),
	)
	if err != nil {
		log.Fatalf("search: %v", err)
	}

	fmt.Println("\nSearch results:")
	for _, fact := range knowledge.FactsToStrings(searchResult.Facts) {
		fmt.Printf("  - %s\n", fact)
	}

	// 7. Explore the graph.
	graphData, err := graph.GetGraph(ctx, 50)
	if err != nil {
		log.Fatalf("get graph: %v", err)
	}
	fmt.Printf("\nGraph: %d nodes, %d edges\n", len(graphData.Nodes), len(graphData.Edges))

	// 8. Get node details with neighbors.
	if len(graphData.Nodes) > 0 {
		detail, err := graph.GetNode(ctx, graphData.Nodes[0].ID, 1)
		if err != nil {
			log.Printf("get node: %v", err)
		} else {
			fmt.Printf("Node %q has %d neighbors\n", detail.Node.Name, len(detail.Neighbors))

			sub := knowledge.Subgraph(detail)
			fmt.Printf("Subgraph: %d nodes, %d edges\n", len(sub.Nodes), len(sub.Edges))
		}
	}
}
