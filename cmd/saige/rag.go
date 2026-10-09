package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/urmzd/saige/agent/tui"
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

// openRAGPipeline connects to the RAG database (--db, then SAIGE_RAG_DB) and
// builds the shared pipeline. The returned cleanup closes the pipeline and
// the pool. withEmbedder is set only for commands that embed (search, ingest).
func openRAGPipeline(ctx context.Context, dsn string, withEmbedder bool) (ragtypes.Pipeline, func(), error) {
	if dsn == "" {
		dsn = os.Getenv("SAIGE_RAG_DB")
	}
	if dsn == "" {
		return nil, nil, errors.New("--db or SAIGE_RAG_DB is required")
	}

	pool, err := connectPostgres(ctx, dsn)
	if err != nil {
		return nil, nil, err
	}

	pipeline, err := newRAGPipeline(ctx, pool, persistentFlagVars, withEmbedder)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}

	return pipeline, func() {
		_ = pipeline.Close(ctx)
		pool.Close()
	}, nil
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
				return reported(out, fmt.Errorf("--query is required"))
			}

			pipeline, cleanup, err := openRAGPipeline(ctx, db, true)
			if err != nil {
				return reported(out, err)
			}
			defer cleanup()

			result, err := pipeline.Search(ctx, query, ragtypes.WithLimit(limit))
			if err != nil {
				return reported(out, err)
			}

			if err := out.Result(withoutEmbeddings(result)); err != nil {
				return reported(out, err)
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
				return reported(out, fmt.Errorf("--uuid is required"))
			}

			pipeline, cleanup, err := openRAGPipeline(ctx, db, false)
			if err != nil {
				return reported(out, err)
			}
			defer cleanup()

			hit, err := pipeline.Lookup(ctx, uuid)
			if err != nil {
				return reported(out, err)
			}

			if err := out.Result(hit); err != nil {
				return reported(out, err)
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
				return reported(out, fmt.Errorf("--file is required"))
			}

			data, err := os.ReadFile(file) //nolint:gosec // file path is from CLI flag, not untrusted input
			if err != nil {
				return reported(out, err)
			}

			pipeline, cleanup, err := openRAGPipeline(ctx, db, true)
			if err != nil {
				return reported(out, err)
			}
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
				return reported(out, err)
			}

			if err := out.Result(result); err != nil {
				return reported(out, err)
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
				return reported(out, fmt.Errorf("--uuid is required"))
			}

			pipeline, cleanup, err := openRAGPipeline(ctx, db, false)
			if err != nil {
				return reported(out, err)
			}
			defer cleanup()

			if err := pipeline.Delete(ctx, uuid); err != nil {
				return reported(out, err)
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

// withoutEmbeddings returns a copy of result whose hits carry no embedding
// vectors. A vector is hundreds of floats per hit, which buries the text in
// both output formats and means nothing to a reader.
func withoutEmbeddings(result *ragtypes.SearchPipelineResult) *ragtypes.SearchPipelineResult {
	if result == nil {
		return nil
	}
	out := *result
	out.Hits = make([]ragtypes.SearchHit, len(result.Hits))
	for i, h := range result.Hits {
		h.Variant.Embedding = nil
		out.Hits[i] = h
	}
	return &out
}
