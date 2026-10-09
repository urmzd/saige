package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	_ "github.com/urmzd/saige/agent/eval" // registers the agent scorer kinds
	"github.com/urmzd/saige/eval"
	"github.com/urmzd/saige/eval/store"
	"github.com/urmzd/saige/eval/store/filestore"
)

// newEvalRunsCmd lists stored runs in a results directory.
func newEvalRunsCmd() *cobra.Command {
	var dir, suite string
	var limit int
	cmd := &cobra.Command{
		Use:          "runs",
		Short:        "List evaluation runs in a results store, newest first",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := openResultsStore(dir)
			if err != nil {
				return err
			}
			runs, err := s.ListRuns(cmd.Context(), store.RunFilter{Suite: suite, Limit: limit})
			if err != nil {
				return err
			}
			if persistentFlagVars.isJSON() {
				return json.NewEncoder(os.Stdout).Encode(runs)
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "RUN\tSUITE\tSTATUS\tOUTCOME\tUNITS\tERRORED\tCOST\tCOMMIT\tSTARTED")
			for _, r := range runs {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%d\t%s\t%s\t%s\n",
					r.ID, r.Suite, r.Status, dash(string(r.Outcome)), r.Units, r.ErroredCases,
					formatCost(r.CostUSD), formatCommit(r.Provenance), r.StartedAt.Local().Format(time.DateTime))
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&dir, "store", "", "Results store directory (required)")
	_ = cmd.MarkFlagRequired("store")
	cmd.Flags().StringVar(&suite, "suite", "", "Only runs of this suite")
	cmd.Flags().IntVar(&limit, "limit", 20, "Maximum runs to list, 0 for all")
	return cmd
}

// newEvalShowCmd prints one stored run: its summary, provenance, aggregate
// metrics, and gate violations.
func newEvalShowCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:          "show <run-id>",
		Short:        "Show one evaluation run from a results store",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openResultsStore(dir)
			if err != nil {
				return err
			}
			run, err := s.GetRun(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if persistentFlagVars.isJSON() {
				units, err := s.Units(cmd.Context(), run.ID, store.UnitFilter{})
				if err != nil {
					return err
				}
				return json.NewEncoder(os.Stdout).Encode(struct {
					Run   eval.RunRecord `json:"run"`
					Units []eval.Unit    `json:"units"`
				}{run, units})
			}
			return printRun(run)
		},
	}
	cmd.Flags().StringVar(&dir, "store", "", "Results store directory (required)")
	_ = cmd.MarkFlagRequired("store")
	return cmd
}

// newEvalScorersCmd lists the scorer kinds a suite can declare by name.
func newEvalScorersCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "scorers",
		Short:        "List the built-in scorer kinds that can be declared by name",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(*cobra.Command, []string) error {
			kinds := eval.DefaultRegistry.Kinds()
			if persistentFlagVars.isJSON() {
				return json.NewEncoder(os.Stdout).Encode(kinds)
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "KIND\tDESCRIPTION")
			for _, k := range kinds {
				fmt.Fprintf(w, "%s\t%s\n", k.Kind, k.Description)
			}
			return w.Flush()
		},
	}
}

func openResultsStore(dir string) (store.Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("--store is required")
	}
	if _, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("results store %s: %w", dir, err)
	}
	return filestore.Open(dir)
}

func printRun(r eval.RunRecord) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	row := func(k, v string) { fmt.Fprintf(w, "%s\t%s\n", k, v) }
	row("run", r.ID)
	row("suite", r.Suite)
	if r.Claim != "" {
		row("claim", r.Claim)
	}
	row("status", string(r.Status))
	row("outcome", dash(string(r.Outcome)))
	if r.Error != "" {
		row("error", r.Error)
	}
	row("started", r.StartedAt.Local().Format(time.DateTime))
	if !r.FinishedAt.IsZero() {
		row("duration", r.FinishedAt.Sub(r.StartedAt).Round(time.Millisecond).String())
	}
	row("units", fmt.Sprintf("%d (errored %d, subject errors %d, incomplete %d, unstable scores %d)",
		r.Units, r.ErroredCases, r.SubjectErrors, r.Incomplete, r.UnstableScores))
	row("cost", formatCost(r.CostUSD))
	row("commit", formatCommit(r.Provenance))
	if len(r.Provenance.Models) > 0 {
		row("models", strings.Join(r.Provenance.Models, ", "))
	}
	if err := w.Flush(); err != nil {
		return err
	}

	if len(r.Aggregate) > 0 {
		fmt.Println()
		names := make([]string, 0, len(r.Aggregate))
		for name := range r.Aggregate {
			names = append(names, name)
		}
		sort.Strings(names)
		w = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "METRIC\tMEAN")
		for _, name := range names {
			fmt.Fprintf(w, "%s\t%.4g\n", name, r.Aggregate[name])
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}
	if len(r.Violations) > 0 {
		fmt.Println()
		fmt.Println("violations:")
		for _, v := range r.Violations {
			fmt.Println("  " + v.String())
		}
	}
	return nil
}

func formatCost(c *float64) string {
	if c == nil {
		return "-"
	}
	return fmt.Sprintf("$%.4f", *c)
}

func formatCommit(p eval.Provenance) string {
	if p.GitCommit == "" {
		return "-"
	}
	commit := p.GitCommit
	if len(commit) > 12 {
		commit = commit[:12]
	}
	if p.GitDirty {
		commit += "+dirty"
	}
	return commit
}
