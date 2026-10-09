package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/urmzd/saige/agent/tui"
	kgtypes "github.com/urmzd/saige/rag/knowledge/types"
)

func newKgCmd(ctx context.Context) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kg",
		Short: "Knowledge graph operations (search, ingest, graph, node)",
	}

	cmd.AddCommand(
		newKgSearchCmd(ctx),
		newKgIngestCmd(ctx),
		newKgGraphCmd(ctx),
		newKgNodeCmd(ctx),
	)

	return cmd
}

// openKnowledgeGraph connects to the knowledge graph database (--db, then
// SAIGE_KG_DB) and builds the shared graph. The returned cleanup closes the
// graph and the pool. withEmbedder is set only for commands that embed (search,
// ingest).
func openKnowledgeGraph(ctx context.Context, dsn string, withEmbedder bool) (kgtypes.Graph, func(), error) {
	if dsn == "" {
		dsn = os.Getenv("SAIGE_KG_DB")
	}
	if dsn == "" {
		return nil, nil, errors.New("--db or SAIGE_KG_DB is required")
	}

	pool, err := connectPostgres(ctx, dsn)
	if err != nil {
		return nil, nil, err
	}

	graph, err := newKnowledgeGraph(ctx, pool, persistentFlagVars, withEmbedder)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}

	return graph, func() {
		_ = graph.Close(ctx)
		pool.Close()
	}, nil
}

func newKgSearchCmd(ctx context.Context) *cobra.Command {
	var db, query, tmplName string
	var limit int

	cmd := &cobra.Command{
		Use:   "search",
		Short: "Search knowledge graph facts",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := tui.ResolveOutput(persistentFlagVars.isJSON(), tui.TemplateByName(tmplName))
			out.Header(tui.OutputHeader{Operation: "kg search"})

			if query == "" {
				return reported(out, fmt.Errorf("--query is required"))
			}

			graph, cleanup, err := openKnowledgeGraph(ctx, db, true)
			if err != nil {
				return reported(out, err)
			}
			defer cleanup()

			// Partial failures still carry usable results: warn and print them.
			result, err := graph.SearchFacts(ctx, query, kgtypes.WithLimit(limit))
			if err != nil {
				if !errors.Is(err, kgtypes.ErrPartialSearch) {
					return reported(out, err)
				}
				fmt.Fprintf(os.Stderr, "warning: %v\n", err)
			}

			if err := out.Result(result); err != nil {
				return reported(out, err)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&db, "db", "", "Postgres DSN [$SAIGE_KG_DB]")
	cmd.Flags().StringVar(&query, "query", "", "Search query")
	cmd.Flags().IntVar(&limit, "limit", 10, "Max results")
	cmd.Flags().StringVar(&tmplName, "template", "default", "Output template (default|minimal|detailed)")

	return cmd
}

func newKgIngestCmd(ctx context.Context) *cobra.Command {
	var db, name, text, source, tmplName string

	cmd := &cobra.Command{
		Use:   "ingest",
		Short: "Ingest text into the graph",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := tui.ResolveOutput(persistentFlagVars.isJSON(), tui.TemplateByName(tmplName))
			out.Header(tui.OutputHeader{Operation: "kg ingest"})

			if name == "" || text == "" {
				return reported(out, fmt.Errorf("--name and --text are required"))
			}

			graph, cleanup, err := openKnowledgeGraph(ctx, db, true)
			if err != nil {
				return reported(out, err)
			}
			defer cleanup()

			result, err := graph.IngestEpisode(ctx, &kgtypes.EpisodeInput{
				Name:   name,
				Body:   text,
				Source: source,
			})
			if err != nil {
				// A partial episode is stored; some extracted facts were not.
				// Report the result and warn rather than fail.
				if !errors.Is(err, kgtypes.ErrPartialEpisode) || result == nil {
					return reported(out, err)
				}
				out.Status("warning: " + err.Error())
			}

			if err := out.Result(result); err != nil {
				return reported(out, err)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&db, "db", "", "Postgres DSN [$SAIGE_KG_DB]")
	cmd.Flags().StringVar(&name, "name", "", "Episode name")
	cmd.Flags().StringVar(&text, "text", "", "Text content to ingest")
	cmd.Flags().StringVar(&source, "source", "", "Source description")
	cmd.Flags().StringVar(&tmplName, "template", "default", "Output template (default|minimal|detailed)")

	return cmd
}

func newKgGraphCmd(ctx context.Context) *cobra.Command {
	var db, tmplName string
	var limit int

	cmd := &cobra.Command{
		Use:   "graph",
		Short: "Export full graph data",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := tui.ResolveOutput(persistentFlagVars.isJSON(), tui.TemplateByName(tmplName))
			out.Header(tui.OutputHeader{Operation: "kg graph"})

			graph, cleanup, err := openKnowledgeGraph(ctx, db, false)
			if err != nil {
				return reported(out, err)
			}
			defer cleanup()

			data, err := graph.GetGraph(ctx, int64(limit))
			if err != nil {
				return reported(out, err)
			}

			if err := out.Result(data); err != nil {
				return reported(out, err)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&db, "db", "", "Postgres DSN [$SAIGE_KG_DB]")
	cmd.Flags().IntVar(&limit, "limit", 100, "Max relations to return")
	cmd.Flags().StringVar(&tmplName, "template", "default", "Output template (default|minimal|detailed)")

	return cmd
}

func newKgNodeCmd(ctx context.Context) *cobra.Command {
	var db, id, tmplName string
	var depth int

	cmd := &cobra.Command{
		Use:   "node",
		Short: "Explore a node's neighborhood",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := tui.ResolveOutput(persistentFlagVars.isJSON(), tui.TemplateByName(tmplName))
			out.Header(tui.OutputHeader{Operation: "kg node"})

			if id == "" {
				return reported(out, fmt.Errorf("--id is required"))
			}

			graph, cleanup, err := openKnowledgeGraph(ctx, db, false)
			if err != nil {
				return reported(out, err)
			}
			defer cleanup()

			detail, err := graph.GetNode(ctx, id, depth)
			if err != nil {
				return reported(out, err)
			}

			if err := out.Result(detail); err != nil {
				return reported(out, err)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&db, "db", "", "Postgres DSN [$SAIGE_KG_DB]")
	cmd.Flags().StringVar(&id, "id", "", "Entity UUID")
	cmd.Flags().IntVar(&depth, "depth", 1, "Traversal depth")
	cmd.Flags().StringVar(&tmplName, "template", "default", "Output template (default|minimal|detailed)")

	return cmd
}
