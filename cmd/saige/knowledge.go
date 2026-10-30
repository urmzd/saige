package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func newKnowledgeCmd(ctx context.Context) *cobra.Command {
	cmd := &cobra.Command{Use: "knowledge", Short: "Ingest, retrieve and inspect knowledge"}
	cmd.AddCommand(knowledgeOperation("search", newRagSearchCmd(ctx), newKgSearchCmd(ctx)), knowledgeOperation("ingest", newRagIngestCmd(ctx), newKgIngestCmd(ctx)))
	for _, operation := range []*cobra.Command{newRagLookupCmd(ctx), newRagSyncCmd(ctx), newRagDeleteCmd(ctx)} {
		name := operation.Name()
		if name == "lookup" {
			name = "show"
		}
		cmd.AddCommand(knowledgeOperation(name, operation, nil))
	}
	graph := &cobra.Command{Use: "graph", Short: "Inspect the knowledge graph (KG backend)"}
	export, neighbors := newKgGraphCmd(ctx), newKgNodeCmd(ctx)
	export.Use, neighbors.Use = "export", "neighbors"
	export.Args, neighbors.Args = cobra.NoArgs, cobra.NoArgs
	graph.AddCommand(export, neighbors)
	cmd.AddCommand(graph)
	return cmd
}

// Share the operation surface while retaining each backend's flags and
// validation. Unsupported flags fail before a database is opened.
func knowledgeOperation(name string, rag, kg *cobra.Command) *cobra.Command {
	var backend string
	cmd := &cobra.Command{Use: name, Short: rag.Short, Args: cobra.NoArgs}
	if kg != nil {
		cmd.Short += " (RAG or KG backend)"
	}
	cmd.Flags().AddFlagSet(rag.Flags())
	if kg != nil {
		kg.Flags().VisitAll(func(f *pflag.Flag) {
			if cmd.Flags().Lookup(f.Name) == nil {
				cmd.Flags().AddFlag(f)
			}
		})
	}
	if kg != nil {
		cmd.Flags().VisitAll(func(f *pflag.Flag) {
			switch {
			case f.Name == "db":
				f.Usage = "Postgres DSN [$SAIGE_RAG_DB or $SAIGE_KG_DB, selected by --backend]"
			case rag.Flags().Lookup(f.Name) == nil:
				f.Usage += " (KG only)"
			case kg.Flags().Lookup(f.Name) == nil:
				f.Usage += " (RAG only)"
			}
		})
	}
	cmd.Flags().StringVar(&backend, "backend", "rag", "Knowledge backend: rag or kg (KG supports search and ingest)")
	cmd.RunE = func(c *cobra.Command, args []string) error {
		selected := rag
		switch backend {
		case "rag":
		case "kg":
			selected = kg
		default:
			return fmt.Errorf("unknown knowledge backend %q: choose rag or kg", backend)
		}
		if selected == nil {
			return fmt.Errorf("knowledge %s does not support backend %s", name, backend)
		}
		var flagErr error
		c.Flags().Visit(func(f *pflag.Flag) {
			if flagErr != nil || f.Name == "backend" || rag.Flags().Lookup(f.Name) == nil && (kg == nil || kg.Flags().Lookup(f.Name) == nil) {
				return
			}
			target := selected.Flags().Lookup(f.Name)
			if target == nil {
				flagErr = fmt.Errorf("--%s is not supported by backend %s", f.Name, backend)
				return
			}
			if target != f {
				flagErr = selected.Flags().Set(f.Name, f.Value.String())
			}
		})
		if flagErr != nil {
			return flagErr
		}
		return selected.RunE(c, args)
	}
	return cmd
}
