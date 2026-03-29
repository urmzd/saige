package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/urmzd/saige/agent/tui"
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
	jsonMode := fs.Bool("json", false, "Output as JSON (no styling)")
	tmplName := fs.String("template", "default", "Output template (default|minimal|detailed)")
	_ = fs.Parse(args)

	out := tui.ResolveOutput(*jsonMode, tui.TemplateByName(*tmplName))
	out.Header(tui.OutputHeader{Operation: "rag search"})

	if *query == "" {
		out.Error(fmt.Errorf("--query is required"))
		os.Exit(1)
	}

	pipeline, cleanup := ragPipeline(ctx, *db)
	defer cleanup()

	result, err := pipeline.Search(ctx, *query, ragtypes.WithLimit(*limit))
	if err != nil {
		out.Error(err)
		os.Exit(1)
	}

	if err := out.Result(result); err != nil {
		out.Error(err)
		os.Exit(1)
	}
}

func ragLookup(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("rag lookup", flag.ExitOnError)
	db := fs.String("db", "", "Postgres DSN [$SAIGE_RAG_DB]")
	uuid := fs.String("uuid", "", "Variant UUID")
	jsonMode := fs.Bool("json", false, "Output as JSON (no styling)")
	tmplName := fs.String("template", "default", "Output template (default|minimal|detailed)")
	_ = fs.Parse(args)

	out := tui.ResolveOutput(*jsonMode, tui.TemplateByName(*tmplName))
	out.Header(tui.OutputHeader{Operation: "rag lookup"})

	if *uuid == "" {
		out.Error(fmt.Errorf("--uuid is required"))
		os.Exit(1)
	}

	pipeline, cleanup := ragPipeline(ctx, *db)
	defer cleanup()

	hit, err := pipeline.Lookup(ctx, *uuid)
	if err != nil {
		out.Error(err)
		os.Exit(1)
	}

	if err := out.Result(hit); err != nil {
		out.Error(err)
		os.Exit(1)
	}
}

func ragIngest(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("rag ingest", flag.ExitOnError)
	db := fs.String("db", "", "Postgres DSN [$SAIGE_RAG_DB]")
	file := fs.String("file", "", "File path to ingest")
	mime := fs.String("mime", "text/plain", "MIME type")
	source := fs.String("source", "", "Source URI")
	jsonMode := fs.Bool("json", false, "Output as JSON (no styling)")
	tmplName := fs.String("template", "default", "Output template (default|minimal|detailed)")
	_ = fs.Parse(args)

	out := tui.ResolveOutput(*jsonMode, tui.TemplateByName(*tmplName))
	out.Header(tui.OutputHeader{Operation: "rag ingest"})

	if *file == "" {
		out.Error(fmt.Errorf("--file is required"))
		os.Exit(1)
	}

	data, err := os.ReadFile(*file)
	if err != nil {
		out.Error(err)
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
		out.Error(err)
		os.Exit(1)
	}

	if err := out.Result(result); err != nil {
		out.Error(err)
		os.Exit(1)
	}
}

func ragDelete(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("rag delete", flag.ExitOnError)
	db := fs.String("db", "", "Postgres DSN [$SAIGE_RAG_DB]")
	uuid := fs.String("uuid", "", "Document UUID")
	jsonMode := fs.Bool("json", false, "Output as JSON (no styling)")
	tmplName := fs.String("template", "default", "Output template (default|minimal|detailed)")
	_ = fs.Parse(args)

	out := tui.ResolveOutput(*jsonMode, tui.TemplateByName(*tmplName))
	out.Header(tui.OutputHeader{Operation: "rag delete"})

	if *uuid == "" {
		out.Error(fmt.Errorf("--uuid is required"))
		os.Exit(1)
	}

	pipeline, cleanup := ragPipeline(ctx, *db)
	defer cleanup()

	if err := pipeline.Delete(ctx, *uuid); err != nil {
		out.Error(err)
		os.Exit(1)
	}

	out.Status(fmt.Sprintf("deleted %s", *uuid))
}
