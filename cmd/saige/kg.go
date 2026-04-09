package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/urmzd/saige/agent/tui"
	"github.com/urmzd/saige/knowledge"
	kgtypes "github.com/urmzd/saige/knowledge/types"
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

func newKgSearchCmd(ctx context.Context) *cobra.Command {
	var db, query, tmplName string
	var limit int
	var jsonMode bool

	cmd := &cobra.Command{
		Use:   "search",
		Short: "Search knowledge graph facts",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := tui.ResolveOutput(jsonMode, tui.TemplateByName(tmplName))
			out.Header(tui.OutputHeader{Operation: "kg search"})

			if query == "" {
				out.Error(fmt.Errorf("--query is required"))
				os.Exit(1)
			}

			graph, cleanup := kgGraph_(ctx, db)
			defer cleanup()

			result, err := graph.SearchFacts(ctx, query, kgtypes.WithLimit(limit))
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

	cmd.Flags().StringVar(&db, "db", "", "Postgres DSN [$SAIGE_KG_DB]")
	cmd.Flags().StringVar(&query, "query", "", "Search query")
	cmd.Flags().IntVar(&limit, "limit", 10, "Max results")
	cmd.Flags().BoolVar(&jsonMode, "json", false, "Output as JSON (no styling)")
	cmd.Flags().StringVar(&tmplName, "template", "default", "Output template (default|minimal|detailed)")

	return cmd
}

func newKgIngestCmd(ctx context.Context) *cobra.Command {
	var db, name, text, source, tmplName string
	var jsonMode bool

	cmd := &cobra.Command{
		Use:   "ingest",
		Short: "Ingest text into the graph",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := tui.ResolveOutput(jsonMode, tui.TemplateByName(tmplName))
			out.Header(tui.OutputHeader{Operation: "kg ingest"})

			if name == "" || text == "" {
				out.Error(fmt.Errorf("--name and --text are required"))
				os.Exit(1)
			}

			graph, cleanup := kgGraph_(ctx, db)
			defer cleanup()

			result, err := graph.IngestEpisode(ctx, &kgtypes.EpisodeInput{
				Name:   name,
				Body:   text,
				Source: source,
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

	cmd.Flags().StringVar(&db, "db", "", "Postgres DSN [$SAIGE_KG_DB]")
	cmd.Flags().StringVar(&name, "name", "", "Episode name")
	cmd.Flags().StringVar(&text, "text", "", "Text content to ingest")
	cmd.Flags().StringVar(&source, "source", "", "Source description")
	cmd.Flags().BoolVar(&jsonMode, "json", false, "Output as JSON (no styling)")
	cmd.Flags().StringVar(&tmplName, "template", "default", "Output template (default|minimal|detailed)")

	return cmd
}

func newKgGraphCmd(ctx context.Context) *cobra.Command {
	var db, tmplName string
	var limit int
	var jsonMode bool

	cmd := &cobra.Command{
		Use:   "graph",
		Short: "Export full graph data",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := tui.ResolveOutput(jsonMode, tui.TemplateByName(tmplName))
			out.Header(tui.OutputHeader{Operation: "kg graph"})

			graph, cleanup := kgGraph_(ctx, db)
			defer cleanup()

			data, err := graph.GetGraph(ctx, int64(limit))
			if err != nil {
				out.Error(err)
				os.Exit(1)
			}

			if err := out.Result(data); err != nil {
				out.Error(err)
				os.Exit(1)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&db, "db", "", "Postgres DSN [$SAIGE_KG_DB]")
	cmd.Flags().IntVar(&limit, "limit", 100, "Max relations to return")
	cmd.Flags().BoolVar(&jsonMode, "json", false, "Output as JSON (no styling)")
	cmd.Flags().StringVar(&tmplName, "template", "default", "Output template (default|minimal|detailed)")

	return cmd
}

func newKgNodeCmd(ctx context.Context) *cobra.Command {
	var db, id, tmplName string
	var depth int
	var jsonMode bool

	cmd := &cobra.Command{
		Use:   "node",
		Short: "Explore a node's neighborhood",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := tui.ResolveOutput(jsonMode, tui.TemplateByName(tmplName))
			out.Header(tui.OutputHeader{Operation: "kg node"})

			if id == "" {
				out.Error(fmt.Errorf("--id is required"))
				os.Exit(1)
			}

			graph, cleanup := kgGraph_(ctx, db)
			defer cleanup()

			detail, err := graph.GetNode(ctx, id, depth)
			if err != nil {
				out.Error(err)
				os.Exit(1)
			}

			if err := out.Result(detail); err != nil {
				out.Error(err)
				os.Exit(1)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&db, "db", "", "Postgres DSN [$SAIGE_KG_DB]")
	cmd.Flags().StringVar(&id, "id", "", "Entity UUID")
	cmd.Flags().IntVar(&depth, "depth", 1, "Traversal depth")
	cmd.Flags().BoolVar(&jsonMode, "json", false, "Output as JSON (no styling)")
	cmd.Flags().StringVar(&tmplName, "template", "default", "Output template (default|minimal|detailed)")

	return cmd
}
