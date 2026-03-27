package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/urmzd/saige/knowledge"
	kgtypes "github.com/urmzd/saige/knowledge/types"
)

func runKG(ctx context.Context, args []string) {
	if len(args) < 1 {
		printKGUsage()
		os.Exit(1)
	}

	switch args[0] {
	case "search":
		kgSearch(ctx, args[1:])
	case "ingest":
		kgIngest(ctx, args[1:])
	case "graph":
		kgGraph(ctx, args[1:])
	case "node":
		kgNode(ctx, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown kg command: %s\n", args[0])
		printKGUsage()
		os.Exit(1)
	}
}

func printKGUsage() {
	fmt.Fprintln(os.Stderr, `Usage: saige kg <command> [flags]

Commands:
  search   Search knowledge graph facts
  ingest   Ingest text into the graph
  graph    Export full graph data
  node     Explore a node's neighborhood`)
}

func kgGraph_(ctx context.Context, dsn string) (kgtypes.Graph, func()) {
	if dsn == "" {
		dsn = os.Getenv("SAIGE_KG_DB")
	}
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "error: --db or SAIGE_KG_DB is required")
		os.Exit(1)
	}

	pool, err := connectPostgres(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	graph, err := knowledge.NewGraph(ctx, knowledge.WithPostgres(pool))
	if err != nil {
		pool.Close()
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	return graph, func() {
		_ = graph.Close(ctx)
		pool.Close()
	}
}

func kgSearch(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("kg search", flag.ExitOnError)
	db := fs.String("db", "", "Postgres DSN [$SAIGE_KG_DB]")
	query := fs.String("query", "", "Search query")
	limit := fs.Int("limit", 10, "Max results")
	_ = fs.Parse(args)

	if *query == "" {
		fmt.Fprintln(os.Stderr, "error: --query is required")
		os.Exit(1)
	}

	graph, cleanup := kgGraph_(ctx, *db)
	defer cleanup()

	result, err := graph.SearchFacts(ctx, *query, kgtypes.WithLimit(*limit))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	printJSON(result)
}

func kgIngest(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("kg ingest", flag.ExitOnError)
	db := fs.String("db", "", "Postgres DSN [$SAIGE_KG_DB]")
	name := fs.String("name", "", "Episode name")
	text := fs.String("text", "", "Text content to ingest")
	source := fs.String("source", "", "Source description")
	_ = fs.Parse(args)

	if *name == "" || *text == "" {
		fmt.Fprintln(os.Stderr, "error: --name and --text are required")
		os.Exit(1)
	}

	graph, cleanup := kgGraph_(ctx, *db)
	defer cleanup()

	result, err := graph.IngestEpisode(ctx, &kgtypes.EpisodeInput{
		Name:   *name,
		Body:   *text,
		Source: *source,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	printJSON(result)
}

func kgGraph(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("kg graph", flag.ExitOnError)
	db := fs.String("db", "", "Postgres DSN [$SAIGE_KG_DB]")
	limit := fs.Int("limit", 100, "Max relations to return")
	_ = fs.Parse(args)

	graph, cleanup := kgGraph_(ctx, *db)
	defer cleanup()

	data, err := graph.GetGraph(ctx, int64(*limit))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	printJSON(data)
}

func kgNode(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("kg node", flag.ExitOnError)
	db := fs.String("db", "", "Postgres DSN [$SAIGE_KG_DB]")
	id := fs.String("id", "", "Entity UUID")
	depth := fs.Int("depth", 1, "Traversal depth")
	_ = fs.Parse(args)

	if *id == "" {
		fmt.Fprintln(os.Stderr, "error: --id is required")
		os.Exit(1)
	}

	graph, cleanup := kgGraph_(ctx, *db)
	defer cleanup()

	detail, err := graph.GetNode(ctx, *id, *depth)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	printJSON(detail)
}
