package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/eval"
	"github.com/urmzd/saige/eval/online"
	"github.com/urmzd/saige/eval/store"
	"github.com/urmzd/saige/postgres"
)

// evalOnlineFlags holds the flags of saige eval online.
type evalOnlineFlags struct {
	store, tenant, source, scope, suite, channel string
	since                                        time.Duration
	from, to                                     string
	rate                                         float64
	seed                                         uint64
	models, presets, tools, labels, scorers      []string
	promoteMetrics                               []string
	errored                                      string
	judge                                        bool
	judgeRubric                                  string
	judgeBudget                                  float64
	judgeMaxCalls                                int
	promote                                      string
	promoteBelow                                 float64
	watch                                        bool
}

func newEvalOnlineCmd(ctx context.Context) *cobra.Command {
	var f evalOnlineFlags
	cmd := &cobra.Command{
		Use:   "online",
		Short: "Score a sample of finished production runs into a results store",
		Long: `Score a sample of finished production runs into a results store.

Runs are read from conversations stored by agent/pgstore in the PostgreSQL
database named by --source (default: --store when it is a PostgreSQL URL).
A run ends at an assistant turn that calls no tools. The runs that pass the
filters are sampled at --rate with --seed, so the same seed picks the same
runs every time, and scored with each --scorer (a kind from saige eval
scorers, or a JSON scorer spec). --judge adds an LLM judge on the model
selected by --provider and --model; pick a cheap one. Judge calls are
charged to a budget of --judge-budget USD and --judge-max-calls calls, and
are skipped once it is spent.

Results are recorded as one run labeled source=online, with one unit per
scored run whose labels name its conversation and node.

By default the command sweeps one window, the last --since, and exits. With
--watch it keeps running, scoring each run a producer announces on
--channel through PostgreSQL LISTEN/NOTIFY (eval/online.Announce), after
first sweeping the last --since; stop it with Ctrl-C to finish the run.

--promote writes the units that fail (flagged, failed or errored scores, or
a --promote-metric score below --promote-below) as dataset cases in JSON
Lines, with personal data redacted.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runEvalOnline(ctx, cmd, f)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.store, "store", "", storeFlagUsage+" (required)")
	_ = cmd.MarkFlagRequired("store")
	fl.StringVar(&f.tenant, "tenant", "", tenantFlagUsage)
	fl.StringVar(&f.source, "source", "", "PostgreSQL URL of the stored conversations (default: --store when it is a PostgreSQL URL)")
	fl.StringVar(&f.scope, "scope", "", "Read only conversations of this tenant scope (agent/pgstore.NewScopedStore)")
	fl.StringVar(&f.suite, "suite", online.DefaultSuite, "Suite name of the recorded run")
	fl.DurationVar(&f.since, "since", time.Hour, "Window to sweep, ending now (ignored when --from is set)")
	fl.StringVar(&f.from, "from", "", "Window start, RFC 3339")
	fl.StringVar(&f.to, "to", "", "Window end, RFC 3339 (default now)")
	fl.Float64Var(&f.rate, "rate", 1, "Fraction of matching runs to score, from 0 to 1")
	fl.Uint64Var(&f.seed, "seed", 0, "Sampling seed; the same seed samples the same runs")
	fl.StringArrayVar(&f.models, "only-model", nil, "Only runs served by this model (repeatable)")
	fl.StringArrayVar(&f.presets, "only-preset", nil, "Only runs served by this preset (repeatable)")
	fl.StringArrayVar(&f.tools, "tool", nil, "Only runs that called this tool (repeatable)")
	fl.StringArrayVar(&f.labels, "label", nil, "Only runs whose labels match key=value (repeatable)")
	fl.StringVar(&f.errored, "errored", "", "Only runs that did (true) or did not (false) record an error")
	fl.StringArrayVar(&f.scorers, "scorer", nil, `Scorer to run: a kind such as "tool_success_rate", or a JSON spec (repeatable)`)
	fl.BoolVar(&f.judge, "judge", false, "Add an LLM judge on --provider and --model")
	fl.StringVar(&f.judgeRubric, "judge-rubric", "Score 1 when the response correctly and helpfully answers the request, 0 when it does not.", "Rubric for --judge")
	fl.Float64Var(&f.judgeBudget, "judge-budget", 0.10, "Most USD the judge may spend")
	fl.IntVar(&f.judgeMaxCalls, "judge-max-calls", 100, "Most judge calls")
	fl.StringVar(&f.promote, "promote", "", "Write failing units as redacted dataset cases to this JSON Lines file")
	fl.Float64Var(&f.promoteBelow, "promote-below", 0.5, "Score of a --promote-metric below which --promote takes a unit")
	fl.StringArrayVar(&f.promoteMetrics, "promote-metric", []string{"judge_score"}, "Metric on a 0 to 1 scale compared with --promote-below (repeatable)")
	fl.BoolVar(&f.watch, "watch", false, "Keep running and score runs as they are announced")
	fl.StringVar(&f.channel, "channel", online.DefaultChannel, "Notifier channel for --watch")
	return cmd
}

func runEvalOnline(ctx context.Context, cmd *cobra.Command, f evalOnlineFlags) error {
	window, err := onlineWindow(f, time.Now())
	if err != nil {
		return invalidInput(err)
	}
	filter, err := onlineFilter(f)
	if err != nil {
		return invalidInput(err)
	}
	if f.rate < 0 || f.rate > 1 {
		return invalidInput(fmt.Errorf("--rate must be between 0 and 1"))
	}
	scorers, err := buildOnlineScorers(f.scorers)
	if err != nil {
		return invalidInput(err)
	}
	sourceURL := f.source
	if sourceURL == "" && isPostgresStore(f.store) {
		sourceURL = f.store
	}
	if !isPostgresStore(sourceURL) {
		return invalidInput(fmt.Errorf("--source must be a PostgreSQL URL holding the stored conversations"))
	}

	results, closeStore, err := openEvalStore(ctx, f.store, f.tenant, true)
	if err != nil {
		return err
	}
	defer closeStore()
	pool, err := connectPostgres(ctx, sourceURL)
	if err != nil {
		return fmt.Errorf("conversation source: %w", err)
	}
	defer pool.Close()

	s := &online.Sampler{
		Store:      results,
		Suite:      f.suite,
		Rate:       f.rate,
		Seed:       f.seed,
		Filter:     filter,
		Scorers:    scorers,
		Provenance: eval.CaptureProvenance(ctx, ""),
	}
	if f.judge {
		p, err := buildProvider(ctx, persistentFlagVars, false)
		if err != nil {
			return err
		}
		s.Budget = types.NewBudget(types.BudgetPolicy{Limit: types.USD(f.judgeBudget), MaxRequests: f.judgeMaxCalls})
		s.Judges = []eval.Scorer{eval.NewJudgeScorer(&online.BudgetedGenerator{Provider: p, Budget: s.Budget},
			eval.WithJudgeName("judge_score"), eval.WithJudgeRubric(f.judgeRubric))}
		s.Provenance.AddModels(types.ProviderModel(p))
	}
	if len(s.Scorers) == 0 && len(s.Judges) == 0 {
		return invalidInput(fmt.Errorf("nothing to score: pass --scorer or --judge"))
	}
	src := online.PGSource{Pool: pool, Scope: f.scope}

	var rep online.Report
	if f.watch {
		n := postgres.NewNotifier(pool, postgres.NotifierOptions{})
		defer func() { _ = n.Close() }()
		wctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
		rep, err = s.Watch(wctx, n, src, online.WatchOptions{
			Channel: f.channel,
			Since:   window.From,
			OnReady: func(id string) { fmt.Fprintf(os.Stderr, "watching %s as run %s\n", f.channel, id) },
			OnUnit: func(u eval.Unit) {
				fmt.Fprintf(os.Stderr, "scored %s/%s\n", u.Observation.Labels[online.LabelConversation], u.Observation.ID)
			},
		})
	} else {
		rep, err = s.Sweep(ctx, src, window)
	}
	if rep.Run.ID == "" {
		return err
	}
	if f.promote != "" {
		if perr := promoteOnline(ctx, results, rep.Run.ID, f); perr != nil {
			return perr
		}
	}
	if perr := printOnlineReport(cmd, rep, s.Budget); perr != nil {
		return perr
	}
	return err
}

// onlineWindow resolves --from and --to, or --since ending at now.
func onlineWindow(f evalOnlineFlags, now time.Time) (online.Window, error) {
	w := online.Window{From: now.Add(-f.since)}
	if f.from != "" {
		t, err := time.Parse(time.RFC3339, f.from)
		if err != nil {
			return w, fmt.Errorf("--from: %w", err)
		}
		w.From = t
	}
	if f.to != "" {
		t, err := time.Parse(time.RFC3339, f.to)
		if err != nil {
			return w, fmt.Errorf("--to: %w", err)
		}
		w.To = t
	}
	if !w.To.IsZero() && !w.From.Before(w.To) {
		return w, fmt.Errorf("the window is empty: --from must be before --to")
	}
	return w, nil
}

func onlineFilter(f evalOnlineFlags) (online.Filter, error) {
	filter := online.Filter{Models: f.models, Presets: f.presets, Tools: f.tools}
	for _, l := range f.labels {
		k, v, ok := strings.Cut(l, "=")
		if !ok || k == "" {
			return filter, fmt.Errorf("--label %q: want key=value", l)
		}
		if filter.Where == nil {
			filter.Where = eval.Where{}
		}
		filter.Where[k] = v
	}
	if f.errored != "" {
		b, err := strconv.ParseBool(f.errored)
		if err != nil {
			return filter, fmt.Errorf("--errored %q: want true or false", f.errored)
		}
		filter.Errored = &b
	}
	return filter, nil
}

// buildOnlineScorers builds each --scorer, a registered kind or a JSON spec.
func buildOnlineScorers(specs []string) ([]eval.Scorer, error) {
	var out []eval.Scorer
	for _, raw := range specs {
		spec := eval.ScorerSpec{Kind: strings.TrimSpace(raw)}
		if strings.HasPrefix(spec.Kind, "{") {
			if err := json.Unmarshal([]byte(raw), &spec); err != nil {
				return nil, fmt.Errorf("--scorer %s: %w", raw, err)
			}
		}
		s, err := eval.BuildScorer(spec)
		if err != nil {
			return nil, fmt.Errorf("--scorer %s: %w", raw, err)
		}
		out = append(out, s)
	}
	return out, nil
}

func promoteOnline(ctx context.Context, results store.Store, runID string, f evalOnlineFlags) error {
	units, err := results.Units(ctx, runID, store.UnitFilter{})
	if err != nil {
		return err
	}
	cases, err := online.Promote(ctx, units, online.PromoteOptions{Failing: online.Failing(f.promoteBelow, f.promoteMetrics...)})
	if err != nil {
		return err
	}
	out, err := os.OpenFile(f.promote, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if err := online.WriteCases(out, cases); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "promoted %d cases to %s\n", len(cases), f.promote)
	return nil
}

func printOnlineReport(cmd *cobra.Command, rep online.Report, budget *types.Budget) error {
	w := cmd.OutOrStdout()
	if persistentFlagVars.isJSON() {
		return json.NewEncoder(w).Encode(rep)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "run\t%s\n", rep.Run.ID)
	fmt.Fprintf(tw, "status\t%s\n", rep.Run.Status)
	fmt.Fprintf(tw, "runs seen\t%d\n", rep.Seen)
	fmt.Fprintf(tw, "matched\t%d\n", rep.Matched)
	fmt.Fprintf(tw, "scored\t%d\n", rep.Scored)
	if rep.Run.Inconclusive > 0 {
		fmt.Fprintf(tw, "inconclusive\t%d (a scorer failed on infrastructure)\n", rep.Run.Inconclusive)
	}
	if budget != nil {
		fmt.Fprintf(tw, "judge spend\t%s over %d calls (%d skipped)\n", budget.Spent(), budget.Usage().Requests, rep.JudgesSkipped)
	}
	for _, name := range slices.Sorted(maps.Keys(rep.Run.Aggregate)) {
		fmt.Fprintf(tw, "%s\t%.4g\n", name, rep.Run.Aggregate[name])
	}
	return tw.Flush()
}
