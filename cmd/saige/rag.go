package main

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"github.com/urmzd/saige/agent/convert"
	"github.com/urmzd/saige/agent/tui"
	agenttypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/rag"
	"github.com/urmzd/saige/rag/extractor"
	ragtypes "github.com/urmzd/saige/rag/types"
)

// ingestMIME is the MIME type of a file to ingest, from its extension:
// the types the RAG extractors read, then the system's table, then
// text/plain, the ingest default before detection existed.
func ingestMIME(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown":
		return "text/markdown"
	case ".txt", ".text":
		return "text/plain"
	case ".htm", ".html":
		return "text/html"
	case ".pdf":
		return "application/pdf"
	}
	if mt := mime.TypeByExtension(filepath.Ext(path)); mt != "" {
		base, _, _ := strings.Cut(mt, ";")
		return strings.TrimSpace(base)
	}
	return "text/plain"
}

// lazyDescriber describes images through the provider newProvider builds
// on first use, so a document that is not an image needs no credentials.
func lazyDescriber(newProvider func() (agenttypes.Provider, error)) agenttypes.Extractor {
	var (
		once sync.Once
		ex   agenttypes.Extractor
		err  error
	)
	return agenttypes.ExtractorFunc(func(ctx context.Context, data []byte, mt agenttypes.MediaType) ([]agenttypes.UserPart, error) {
		once.Do(func() {
			var p agenttypes.Provider
			if p, err = newProvider(); err == nil {
				ex = convert.AsExtractor(convert.Describe(p))
			}
		})
		if err != nil {
			return nil, fmt.Errorf("describe image: %w", err)
		}
		return ex.Extract(ctx, data, mt)
	})
}

func newRagCmd(ctx context.Context) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rag",
		Short: "RAG document operations (search, lookup, ingest, sync, delete)",
	}

	cmd.AddCommand(
		newRagSearchCmd(ctx),
		newRagLookupCmd(ctx),
		newRagIngestCmd(ctx),
		newRagSyncCmd(ctx),
		newRagDeleteCmd(ctx),
	)

	return cmd
}

// openRAGPipeline connects to the RAG database (--db, then SAIGE_RAG_DB) and
// builds the shared pipeline. The returned cleanup closes the pipeline and
// the pool. withEmbedder is set only for commands that embed (search, ingest).
func openRAGPipeline(ctx context.Context, dsn string, withEmbedder bool, extra ...rag.Option) (ragtypes.Pipeline, func(), error) {
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

	pipeline, err := newRAGPipeline(ctx, pool, persistentFlagVars, withEmbedder, extra...)
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
	var describeImages bool

	cmd := &cobra.Command{
		Use:   "ingest",
		Short: "Ingest a document",
		Long: `Ingest a document.

The MIME type comes from --mime, or else from the file extension (text/plain
when the extension is unknown). Text, Markdown, HTML and PDF documents are
extracted to text. An image (PNG, JPEG, GIF or WebP) needs --describe-images:
the model selected by --provider and --model describes it, and the image is
stored with its description, which is what search matches. Pick a model that
takes images; the description call is billed to that provider.`,
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

			if mime == "" {
				mime = ingestMIME(file)
			}
			var extra []rag.Option
			if describeImages {
				auto := extractor.NewAuto()
				auto.RegisterImages(lazyDescriber(func() (agenttypes.Provider, error) {
					return resolveProvider(ctx, persistentFlagVars, false)
				}))
				extra = append(extra, rag.WithContentExtractor(auto))
			}

			pipeline, cleanup, err := openRAGPipeline(ctx, db, true, extra...)
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
	cmd.Flags().StringVar(&mime, "mime", "", "MIME type (default: from the file extension, else text/plain)")
	cmd.Flags().BoolVar(&describeImages, "describe-images", false, "Ingest images by a description from --provider and --model")
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
