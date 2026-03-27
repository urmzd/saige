package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/urmzd/saige/rag"
	"github.com/urmzd/saige/rag/extractor"
	"github.com/urmzd/saige/rag/pgstore"
	ragtypes "github.com/urmzd/saige/rag/types"
)

func runRAG(ctx context.Context, args []string) {
	if len(args) < 1 {
		printRAGUsage()
		os.Exit(1)
	}

	switch args[0] {
	case "search":
		ragSearch(ctx, args[1:])
	case "lookup":
		ragLookup(ctx, args[1:])
	case "ingest":
		ragIngest(ctx, args[1:])
	case "delete":
		ragDelete(ctx, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown rag command: %s\n", args[0])
		printRAGUsage()
		os.Exit(1)
	}
}

func printRAGUsage() {
	fmt.Fprintln(os.Stderr, `Usage: saige rag <command> [flags]

Commands:
  search   Search documents
  lookup   Get variant by UUID
  ingest   Ingest a document
  delete   Delete a document`)
}

func ragPipeline(ctx context.Context, dsn string) (ragtypes.Pipeline, func()) {
	if dsn == "" {
		dsn = os.Getenv("SAIGE_RAG_DB")
	}
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "error: --db or SAIGE_RAG_DB is required")
		os.Exit(1)
	}

	pool, err := connectPostgres(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	store := pgstore.NewStore(pool, nil)
	pipeline, err := rag.NewPipeline(
		rag.WithStore(store),
		rag.WithContentExtractor(extractor.NewAuto()),
		rag.WithRecursiveChunker(512, 64),
		rag.WithBM25(nil),
	)
	if err != nil {
		pool.Close()
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	return pipeline, func() {
		_ = pipeline.Close(ctx)
		pool.Close()
	}
}

func ragSearch(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("rag search", flag.ExitOnError)
	db := fs.String("db", "", "Postgres DSN [$SAIGE_RAG_DB]")
	query := fs.String("query", "", "Search query")
	limit := fs.Int("limit", 10, "Max results")
	_ = fs.Parse(args)

	if *query == "" {
		fmt.Fprintln(os.Stderr, "error: --query is required")
		os.Exit(1)
	}

	pipeline, cleanup := ragPipeline(ctx, *db)
	defer cleanup()

	result, err := pipeline.Search(ctx, *query, ragtypes.WithLimit(*limit))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	printJSON(result)
}

func ragLookup(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("rag lookup", flag.ExitOnError)
	db := fs.String("db", "", "Postgres DSN [$SAIGE_RAG_DB]")
	uuid := fs.String("uuid", "", "Variant UUID")
	_ = fs.Parse(args)

	if *uuid == "" {
		fmt.Fprintln(os.Stderr, "error: --uuid is required")
		os.Exit(1)
	}

	pipeline, cleanup := ragPipeline(ctx, *db)
	defer cleanup()

	hit, err := pipeline.Lookup(ctx, *uuid)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	printJSON(hit)
}

func ragIngest(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("rag ingest", flag.ExitOnError)
	db := fs.String("db", "", "Postgres DSN [$SAIGE_RAG_DB]")
	file := fs.String("file", "", "File path to ingest")
	mime := fs.String("mime", "text/plain", "MIME type")
	source := fs.String("source", "", "Source URI")
	_ = fs.Parse(args)

	if *file == "" {
		fmt.Fprintln(os.Stderr, "error: --file is required")
		os.Exit(1)
	}

	data, err := os.ReadFile(*file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	pipeline, cleanup := ragPipeline(ctx, *db)
	defer cleanup()

	sourceURI := *source
	if sourceURI == "" {
		sourceURI = "file://" + *file
	}

	result, err := pipeline.Ingest(ctx, &ragtypes.RawDocument{
		SourceURI: sourceURI,
		MIMEType:  *mime,
		Data:      data,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	printJSON(result)
}

func ragDelete(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("rag delete", flag.ExitOnError)
	db := fs.String("db", "", "Postgres DSN [$SAIGE_RAG_DB]")
	uuid := fs.String("uuid", "", "Document UUID")
	_ = fs.Parse(args)

	if *uuid == "" {
		fmt.Fprintln(os.Stderr, "error: --uuid is required")
		os.Exit(1)
	}

	pipeline, cleanup := ragPipeline(ctx, *db)
	defer cleanup()

	if err := pipeline.Delete(ctx, *uuid); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	printJSON(map[string]string{"status": "deleted", "uuid": *uuid})
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
