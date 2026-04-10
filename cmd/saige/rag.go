package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/urmzd/saige/agent/tui"
	"github.com/urmzd/saige/rag"
	"github.com/urmzd/saige/rag/extractor"
	"github.com/urmzd/saige/rag/pgstore"
	ragtypes "github.com/urmzd/saige/rag/types"
)

func newRagCmd(ctx context.Context) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rag",
		Short: "RAG document operations (search, lookup, ingest, delete)",
	}

	cmd.AddCommand(
		newRagSearchCmd(ctx),
		newRagLookupCmd(ctx),
		newRagIngestCmd(ctx),
		newRagDeleteCmd(ctx),
	)

	return cmd
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

func newRagSearchCmd(ctx context.Context) *cobra.Command {
	var db, query, tmplName string
	var limit int

	cmd := &cobra.Command{
		Use:   "search",
		Short: "Search documents",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := tui.ResolveOutput(persistentFlagVars.isJSON(), tui.TemplateByName(tmplName))
			out.Header(tui.OutputHeader{Operation: "rag search"})

			if query == "" {
				out.Error(fmt.Errorf("--query is required"))
				os.Exit(1)
			}

			pipeline, cleanup := ragPipeline(ctx, db)
			defer cleanup()

			result, err := pipeline.Search(ctx, query, ragtypes.WithLimit(limit))
			if err != nil {
				out.Error(err)
				os.Exit(1)
			}

			if err := out.Result(result); err != nil {
				out.Error(err)
				os.Exit(1)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&db, "db", "", "Postgres DSN [$SAIGE_RAG_DB]")
	cmd.Flags().StringVar(&query, "query", "", "Search query")
	cmd.Flags().IntVar(&limit, "limit", 10, "Max results")
	cmd.Flags().StringVar(&tmplName, "template", "default", "Output template (default|minimal|detailed)")

	return cmd
}

func newRagLookupCmd(ctx context.Context) *cobra.Command {
	var db, uuid, tmplName string

	cmd := &cobra.Command{
		Use:   "lookup",
		Short: "Get variant by UUID",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := tui.ResolveOutput(persistentFlagVars.isJSON(), tui.TemplateByName(tmplName))
			out.Header(tui.OutputHeader{Operation: "rag lookup"})

			if uuid == "" {
				out.Error(fmt.Errorf("--uuid is required"))
				os.Exit(1)
			}

			pipeline, cleanup := ragPipeline(ctx, db)
			defer cleanup()

			hit, err := pipeline.Lookup(ctx, uuid)
			if err != nil {
				out.Error(err)
				os.Exit(1)
			}

			if err := out.Result(hit); err != nil {
				out.Error(err)
				os.Exit(1)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&db, "db", "", "Postgres DSN [$SAIGE_RAG_DB]")
	cmd.Flags().StringVar(&uuid, "uuid", "", "Variant UUID")
	cmd.Flags().StringVar(&tmplName, "template", "default", "Output template (default|minimal|detailed)")

	return cmd
}

func newRagIngestCmd(ctx context.Context) *cobra.Command {
	var db, file, mime, source, tmplName string

	cmd := &cobra.Command{
		Use:   "ingest",
		Short: "Ingest a document",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := tui.ResolveOutput(persistentFlagVars.isJSON(), tui.TemplateByName(tmplName))
			out.Header(tui.OutputHeader{Operation: "rag ingest"})

			if file == "" {
				out.Error(fmt.Errorf("--file is required"))
				os.Exit(1)
			}

			data, err := os.ReadFile(file) //nolint:gosec // file path is from CLI flag, not untrusted input
			if err != nil {
				out.Error(err)
				os.Exit(1)
			}

			pipeline, cleanup := ragPipeline(ctx, db)
			defer cleanup()

			sourceURI := source
			if sourceURI == "" {
				sourceURI = "file://" + file
			}

			result, err := pipeline.Ingest(ctx, &ragtypes.RawDocument{
				SourceURI: sourceURI,
				MIMEType:  mime,
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
			return nil
		},
	}

	cmd.Flags().StringVar(&db, "db", "", "Postgres DSN [$SAIGE_RAG_DB]")
	cmd.Flags().StringVar(&file, "file", "", "File path to ingest")
	cmd.Flags().StringVar(&mime, "mime", "text/plain", "MIME type")
	cmd.Flags().StringVar(&source, "source", "", "Source URI")
	cmd.Flags().StringVar(&tmplName, "template", "default", "Output template (default|minimal|detailed)")

	return cmd
}

func newRagDeleteCmd(ctx context.Context) *cobra.Command {
	var db, uuid, tmplName string

	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete a document",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := tui.ResolveOutput(persistentFlagVars.isJSON(), tui.TemplateByName(tmplName))
			out.Header(tui.OutputHeader{Operation: "rag delete"})

			if uuid == "" {
				out.Error(fmt.Errorf("--uuid is required"))
				os.Exit(1)
			}

			pipeline, cleanup := ragPipeline(ctx, db)
			defer cleanup()

			if err := pipeline.Delete(ctx, uuid); err != nil {
				out.Error(err)
				os.Exit(1)
			}

			out.Status(fmt.Sprintf("deleted %s", uuid))
			return nil
		},
	}

	cmd.Flags().StringVar(&db, "db", "", "Postgres DSN [$SAIGE_RAG_DB]")
	cmd.Flags().StringVar(&uuid, "uuid", "", "Document UUID")
	cmd.Flags().StringVar(&tmplName, "template", "default", "Output template (default|minimal|detailed)")

	return cmd
}
