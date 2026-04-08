package main

import (
	"context"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"

	knowledge "github.com/urmzd/saige/knowledge"
	kgtypes "github.com/urmzd/saige/knowledge/types"
)

// mustGraph creates a knowledge graph backed by the given pool, or exits on error.
func mustGraph(ctx context.Context, pool *pgxpool.Pool) kgtypes.Graph {
	graph, err := knowledge.NewGraph(ctx, knowledge.WithPostgres(pool))
	if err != nil {
		log.Fatalf("create knowledge graph: %v", err)
	}
	return graph
}
