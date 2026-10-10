package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/urmzd/saige/eval"
	"github.com/urmzd/saige/eval/harness"
)

// resolveStoreSpec returns the manifest's store: a PostgreSQL URL as given,
// a directory relative to the manifest.
func resolveStoreSpec(m *harness.Manifest) string {
	if isPostgresStore(m.Store) {
		return m.Store
	}
	return m.Resolve(m.Store)
}

func newEvalCmd(ctx context.Context) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "eval",
		Short: "Run live eval corpora against an OpenAI-compatible API or a saige provider",
	}

	cmd.AddCommand(
		newEvalRunCmd(ctx),
		newEvalInitCmd(),
		newEvalValidateCmd(),
		newEvalRunsCmd(),
		newEvalShowCmd(),
		newEvalCompareCmd(),
		newEvalScorersCmd(),
		newEvalOnlineCmd(ctx),
	)

	return cmd
}

// evalRunFlags holds the flags of saige eval run.
type evalRunFlags struct {
	manifest, experimentsDir, idFilter, flowSpec, model, apiBase, apiKey string
	storeDir, tenant, suite, resume, batchStore                          string
	count, concurrency                                                   int
	force, continueOnError, dryRun, allowUnknownModel, batch             bool
	asserts                                                              []string
	maxInconclusive                                                      float64
}

// evalPlan is the resolved configuration of one eval run: manifest values
// overridden by the flags that were set.
type evalPlan struct {
	corpus          string
	idPrefix        string
	count           int
	flows           []string
	client          evalClientConfig
	store           string
	suite           string
	concurrency     int
	continueOnError bool
	assert          []eval.Assertion
}

func newEvalRunCmd(ctx context.Context) *cobra.Command {
	var f evalRunFlags

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run eval flows over an experiment corpus",
		Long: `Run eval flows over an experiment corpus.

Settings come from a suite manifest (--manifest, or ./` + harness.ManifestFile + ` when
--experiments-dir is not given) and are overridden by the flags that are set.
The corpus and manifest are validated before any model call, and the key and
model are checked up front. --dry-run stops after these checks and prints the
plan.

The model endpoint is an OpenAI-compatible API unless --provider (or the
manifest subject provider) selects a saige provider. For the OpenAI-compatible
endpoint the key is --api-key, the manifest's api_key_env, $SAIGE_EVAL_API_KEY,
or the variable that belongs to the endpoint's host (for example
$OPENAI_API_KEY for api.openai.com and $GROQ_API_KEY for api.groq.com). A key
is never sent to a host it does not belong to. api_key_env may name only the
host's own variable or a $SAIGE_EVAL_* variable. A base_url from the manifest
receives only --api-key or the host's own variable unless --api-base confirms
it.

A script whose requests fail on infrastructure (a rate limit, an outage, a
timeout, bad credentials, an unreachable endpoint, or cancellation) is
inconclusive, not failed: the model never answered. An edit turn lost that way
is recorded as unmeasured instead of as a failed turn.

The command fails with exit status 1 when a script fails for another reason
or an assertion (from the manifest or --assert) is violated by a measured
result, with exit status 3 when nothing failed for real but more than
--max-inconclusive of the scripts are inconclusive (rerun it, for example
with --resume), and with exit status 2 when the manifest or corpus is invalid.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEval(ctx, cmd, f)
		},
	}

	fl := cmd.Flags()
	fl.StringVar(&f.manifest, "manifest", "", "Suite manifest ("+harness.ManifestFile+"); flags that are set override it")
	fl.StringVar(&f.experimentsDir, "experiments-dir", "", "Experiment corpus directory (required without a manifest)")
	fl.IntVar(&f.count, "count", 0, "Maximum experiments to run, 0 for all")
	fl.StringVar(&f.idFilter, "id", "", "Experiment ID prefix filter")
	fl.StringVar(&f.flowSpec, "flows", "base,stateless", "Comma-separated flows to run (base|stateless)")
	fl.StringVar(&f.model, "model", "", "Model name (default "+harness.DefaultModel+" for the OpenAI-compatible endpoint, the provider default otherwise)")
	fl.StringVar(&f.apiBase, "api-base", "",
		"OpenAI-compatible API base URL [$SAIGE_EVAL_API_BASE, $OPENAI_BASE_URL, default "+harness.DefaultAPIBase+"]")
	fl.StringVar(&f.apiKey, "api-key", "",
		"API key for the OpenAI-compatible endpoint [$SAIGE_EVAL_API_KEY, then the key variable of the endpoint's host]")
	fl.BoolVar(&f.force, "force", false, "Re-run experiments even when metrics.json exists")
	fl.BoolVar(&f.continueOnError, "continue-on-error", true,
		"Keep running remaining experiments after one fails; failures are written to <experiment>/error.json")
	fl.IntVar(&f.concurrency, "concurrency", 1, "Experiments to run at once")
	fl.StringVar(&f.storeDir, "store", "",
		"Results store, a directory or a PostgreSQL URL (postgres://...); when set, the run and one unit per flow turn are recorded there for saige eval runs and saige eval show")
	fl.StringVar(&f.tenant, "tenant", "", tenantFlagUsage)
	fl.StringVar(&f.suite, "suite", harness.DefaultSuite, "Suite name for the run recorded with --store")
	fl.StringVar(&f.resume, "resume", "",
		"Continue a stored run: experiments it completed are copied into the new run, the rest run again (needs --store)")
	fl.StringArrayVar(&f.asserts, "assert", nil,
		`Gate on a metric, such as "turn_succeeded>=1" (every turn) or "aggregate:latency_ms<=2000"; repeatable`)
	fl.BoolVar(&f.dryRun, "dry-run", false, "Validate, run the preflight checks, and print the plan without calling a model")
	fl.BoolVar(&f.allowUnknownModel, "allow-unknown-model", false, "Run a model the catalog does not list")
	fl.BoolVar(&f.batch, "batch", false,
		"Send the model calls through the provider's batch API at the batch price (needs a saige provider); results can take hours")
	fl.StringVar(&f.batchStore, "batch-store", "",
		"Directory for batch job records with --batch, so a re-run resumes submitted batches instead of submitting them again")
	fl.Float64Var(&f.maxInconclusive, "max-inconclusive", 0,
		"Largest share of scripts, from 0 to 1, that may be inconclusive (lost to infrastructure failures) while the run still passes; above it the command exits 3")

	return cmd
}

// runEval executes saige eval run.
func runEval(ctx context.Context, cmd *cobra.Command, f evalRunFlags) error {
	plan, err := resolveEvalPlan(cmd, f)
	if err != nil {
		return err
	}
	corpusIssues := harness.ValidateCorpus(plan.corpus)
	printIssues(os.Stderr, corpusIssues, true)
	if err := harness.IssuesError(corpusIssues); err != nil {
		return invalidInput(err)
	}
	flows, err := resolveEvalFlows(strings.Join(plan.flows, ","))
	if err != nil {
		return err
	}
	scripts, err := harness.LoadCorpus(plan.corpus)
	if err != nil {
		return err
	}
	scripts = harness.FilterScripts(scripts, plan.idPrefix, plan.count)
	if len(scripts) == 0 {
		return fmt.Errorf("no experiments in %s match id prefix %q", plan.corpus, plan.idPrefix)
	}

	client, transport, err := buildEvalClient(ctx, plan.client, f.dryRun)
	if err != nil {
		return err
	}
	if err := (eval.GatePolicy{MaxInconclusive: f.maxInconclusive}).Validate(); err != nil {
		return invalidInput(fmt.Errorf("--max-inconclusive: %w", err))
	}
	runner := &harness.Runner{
		MaxInconclusive: f.maxInconclusive,
		Client:          client,
		Flows:           flows,
		Force:           f.force,
		ContinueOnError: plan.continueOnError,
		Concurrency:     plan.concurrency,
		Resume:          f.resume,
		Assert:          plan.assert,
		ReuseMetrics:    true,
	}
	if f.batch && !f.dryRun {
		if err := applyEvalBatch(runner.Client, runner, len(scripts), f.batchStore, plan.suite); err != nil {
			return err
		}
	}
	if runner.Client == nil {
		// A dry run builds no client; the plan only needs the model name.
		runner.Client = &harness.Client{Model: transport.Model}
	}
	if plan.store != "" {
		// A dry run only reads the store (to plan a resume) and never
		// creates it.
		results, closeStore, err := openEvalStore(ctx, plan.store, f.tenant, !f.dryRun)
		switch {
		case err == nil:
			defer closeStore()
			runner.Results = results
		case !f.dryRun || f.resume != "":
			return err
		}
		runner.Suite = plan.suite
		if !f.dryRun {
			runner.Provenance = eval.CaptureProvenance(ctx, "")
		}
	}
	if f.resume != "" && runner.Results == nil {
		return fmt.Errorf("--resume %s needs a results store (--store or the manifest's store)", f.resume)
	}
	if f.dryRun {
		planned, err := runner.Plan(ctx, scripts)
		if err != nil {
			return err
		}
		return printEvalPlan(cmd.OutOrStdout(), transport, plan, planned)
	}
	if len(plan.assert) > 0 {
		runner.OnGated = func(s *eval.SuiteResult) {
			fmt.Fprintf(os.Stderr, "gate: %s\n", s.Outcome)
			if s.Inconclusive > 0 {
				fmt.Fprintf(os.Stderr, "  %d results inconclusive (infrastructure failed)\n", s.Inconclusive)
			}
			for _, v := range s.Violations {
				fmt.Fprintf(os.Stderr, "  %s\n", v)
			}
		}
	}
	err = runner.Run(ctx, scripts)
	if errors.Is(err, harness.ErrInconclusive) {
		return exitError{code: exitInconclusive, err: err}
	}
	return err
}

// resolveEvalPlan merges the manifest, when there is one, with the flags.
// A set flag wins over the manifest value.
func resolveEvalPlan(cmd *cobra.Command, f evalRunFlags) (evalPlan, error) {
	m, fromManifest, err := loadEvalManifest(f)
	if err != nil {
		return evalPlan{}, err
	}
	plan := evalPlan{
		corpus:          m.Resolve(m.Corpus),
		idPrefix:        m.IDPrefix,
		count:           m.Count,
		flows:           m.FlowNames(),
		store:           resolveStoreSpec(m),
		suite:           m.Name,
		concurrency:     m.Policy.Concurrency,
		continueOnError: m.ContinueOnError(),
		assert:          append([]eval.Assertion(nil), m.Assert...),
		client: evalClientConfig{
			provider:          m.Subject.Provider,
			model:             m.Subject.Model,
			baseURL:           m.Subject.BaseURL,
			apiKeyEnv:         m.Subject.APIKeyEnv,
			apiKey:            f.apiKey,
			allowUnknownModel: f.allowUnknownModel,
		},
	}
	applyEvalFlags(cmd, f, fromManifest, &plan)
	if plan.corpus == "" {
		return evalPlan{}, fmt.Errorf("no corpus: pass --experiments-dir or --manifest (or create %s)", harness.ManifestFile)
	}
	for _, spec := range f.asserts {
		a, err := parseEvalAssertion(spec)
		if err != nil {
			return evalPlan{}, err
		}
		plan.assert = append(plan.assert, a)
	}
	if issues := harness.ValidateAssertions(plan.assert); harness.HasErrors(issues) {
		return evalPlan{}, invalidInput(harness.IssuesError(issues))
	}
	if plan.concurrency < 0 {
		return evalPlan{}, fmt.Errorf("--concurrency must be zero or positive")
	}
	return plan, nil
}

// loadEvalManifest loads --manifest, or ./saige.eval.json when neither it
// nor --experiments-dir is given. Without a manifest it returns an empty one
// and false. Warnings are printed; errors fail the load.
func loadEvalManifest(f evalRunFlags) (*harness.Manifest, bool, error) {
	path := f.manifest
	if path == "" && f.experimentsDir == "" {
		if _, err := os.Stat(harness.ManifestFile); err == nil {
			path = harness.ManifestFile
		}
	}
	if path == "" {
		return &harness.Manifest{}, false, nil
	}
	m, issues, err := harness.LoadManifest(path)
	if err != nil {
		return nil, false, err
	}
	printIssues(os.Stderr, issues, true)
	if err := harness.IssuesError(issues); err != nil {
		return nil, false, invalidInput(err)
	}
	return m, true, nil
}

// applyEvalFlags overrides plan with the flags. Without a manifest every
// flag applies, defaults included; with one, only the flags that were set.
func applyEvalFlags(cmd *cobra.Command, f evalRunFlags, fromManifest bool, plan *evalPlan) {
	set := func(name string) bool {
		fl := cmd.Flags().Lookup(name)
		return fl != nil && fl.Changed
	}
	use := func(name string) bool { return !fromManifest || set(name) }
	if use("experiments-dir") {
		plan.corpus = f.experimentsDir
	}
	if use("id") {
		plan.idPrefix = f.idFilter
	}
	if use("count") {
		plan.count = f.count
	}
	if use("flows") {
		plan.flows = strings.Split(f.flowSpec, ",")
	}
	if use("store") {
		plan.store = f.storeDir
	}
	if use("suite") || plan.suite == "" {
		plan.suite = f.suite
	}
	if use("concurrency") {
		plan.concurrency = f.concurrency
	}
	if use("continue-on-error") {
		plan.continueOnError = f.continueOnError
	}
	// --provider is the root command's flag; eval reads it only when set,
	// not from $SAIGE_PROVIDER, so a chat default does not redirect evals.
	if set("provider") {
		plan.client.provider = *persistentFlagVars.provider
	}
	switch {
	case set("model"):
		plan.client.model = f.model
	case plan.client.model == "":
		plan.client.model = *persistentFlagVars.model
	}
	if set("api-base") {
		plan.client.baseURL = f.apiBase
		plan.client.baseURLFromFlag = true
	}
}

// parseEvalAssertion parses "metric>=value", "metric<=value" or
// "metric==value", optionally prefixed with "aggregate:" to gate the mean
// instead of every turn.
func parseEvalAssertion(spec string) (eval.Assertion, error) {
	var a eval.Assertion
	body := strings.TrimSpace(spec)
	if rest, ok := strings.CutPrefix(body, "aggregate:"); ok {
		a.Scope = eval.OnAggregate
		body = rest
	}
	for _, op := range []eval.Op{eval.GTE, eval.LTE, eval.EQ} {
		metric, value, ok := strings.Cut(body, string(op))
		if !ok {
			continue
		}
		threshold, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			return a, fmt.Errorf("--assert %q: threshold %q is not a number", spec, strings.TrimSpace(value))
		}
		a.Metric, a.Op, a.Threshold = strings.TrimSpace(metric), op, threshold
		return a, nil
	}
	return a, fmt.Errorf(`--assert %q: want metric>=value, metric<=value or metric==value, optionally prefixed with "aggregate:"`, spec)
}

// printIssues writes issues one per line. With warningsOnly, errors are
// left to the caller's returned error.
func printIssues(w io.Writer, issues []harness.Issue, warningsOnly bool) {
	for _, is := range issues {
		if warningsOnly && is.Severity == harness.SeverityError {
			continue
		}
		fmt.Fprintln(w, is.String())
	}
}

// printEvalPlan prints what a run would do.
func printEvalPlan(w io.Writer, tr evalTransport, plan evalPlan, scripts []harness.PlannedScript) error {
	calls, toRun, exact := 0, 0, true
	for _, s := range scripts {
		if s.Action == harness.PlanRun {
			toRun++
			calls += s.Calls
			exact = exact && s.Exact
		}
	}
	if persistentFlagVars.isJSON() {
		return json.NewEncoder(w).Encode(struct {
			Transport   evalTransport           `json:"transport"`
			Corpus      string                  `json:"corpus"`
			Flows       []string                `json:"flows"`
			Concurrency int                     `json:"concurrency"`
			Assert      []eval.Assertion        `json:"assert,omitempty"`
			Scripts     []harness.PlannedScript `json:"scripts"`
			Calls       int                     `json:"calls"`
			CallsExact  bool                    `json:"calls_exact"`
		}{tr, plan.corpus, plan.flows, max(plan.concurrency, 1), plan.assert, scripts, calls, exact})
	}
	key := "not needed"
	if tr.KeySource != "" {
		key = "$" + tr.KeySource + " (set)"
		if tr.KeySource == keySourceFlag {
			key = "--api-key"
		}
		if !tr.KeySet {
			key = "$" + tr.KeySource + " (NOT SET)"
		}
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "provider\t%s\n", tr.Provider)
	if tr.BaseURL != "" {
		fmt.Fprintf(tw, "base url\t%s\n", tr.BaseURL)
	}
	fmt.Fprintf(tw, "model\t%s\n", tr.Model)
	fmt.Fprintf(tw, "api key\t%s\n", key)
	fmt.Fprintf(tw, "corpus\t%s\n", plan.corpus)
	fmt.Fprintf(tw, "flows\t%s\n", strings.Join(plan.flows, ", "))
	fmt.Fprintf(tw, "concurrency\t%d\n", max(plan.concurrency, 1))
	for _, a := range plan.assert {
		scope := "every turn"
		if a.Scope == eval.OnAggregate {
			scope = "aggregate"
		}
		fmt.Fprintf(tw, "assert\t%s %s %g (%s)\n", a.Metric, a.Op, a.Threshold, scope)
	}
	fmt.Fprintln(tw)
	fmt.Fprintln(tw, "EXPERIMENT\tTURNS\tACTION\tCALLS")
	for _, s := range scripts {
		c := "-"
		if s.Action == harness.PlanRun {
			c = strconv.Itoa(s.Calls)
			if !s.Exact {
				c += "+"
			}
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", s.ID, s.Turns, s.Action, c)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	approx := ""
	if !exact {
		approx = " at least"
	}
	_, err := fmt.Fprintf(w, "\n%d of %d experiments would run,%s %d chat calls\n", toRun, len(scripts), approx, calls)
	return err
}

// evalInitManifest is the suite manifest saige eval init writes next to the
// sample corpus. Its corpus is the manifest's own directory.
const evalInitManifest = `{
  "version": 1,
  "name": "example",
  "corpus": ".",
  "flows": ["base", "stateless"],
  "subject": {
    "provider": "openai-compatible",
    "model": "gpt-6-luna",
    "api_key_env": "OPENAI_API_KEY"
  },
  "policy": {"concurrency": 2},
  "assert": [
    {"metric": "turn_succeeded", "op": ">=", "threshold": 1}
  ]
}
`

func newEvalInitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "init <dir>",
		Short:        "Scaffold a sample eval corpus and suite manifest",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := args[0]
			expDir := filepath.Join(dir, "001-example")
			if _, err := os.Stat(expDir); err == nil {
				return fmt.Errorf("%s already exists", expDir)
			}

			files := map[string]string{
				"system.md": "You are a careful technical writer. Produce the requested document in Markdown. Return only the document, raw, with no commentary.\n",
				"turn-0.md": "Write a short README for a command line tool called hello that prints a greeting. Include an Overview section and a Usage section.\n",
				"turn-1.md": "Add an Installation section explaining that the tool is installed with go install.\n",
			}
			for name, content := range files {
				path := filepath.Join(expDir, name)
				if err := harness.WriteText(path, content); err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "created %s\n", path)
			}
			manifestPath := filepath.Join(dir, harness.ManifestFile)
			if _, err := os.Stat(manifestPath); err == nil {
				fmt.Fprintf(os.Stderr, "kept existing %s\n", manifestPath)
			} else {
				if err := harness.WriteText(manifestPath, evalInitManifest); err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "created %s\n", manifestPath)
			}
			fmt.Fprintf(os.Stderr, "corpus ready; next:\n  saige eval validate %s\n  saige eval run --manifest %s --dry-run\n  saige eval run --manifest %s\n", dir, manifestPath, manifestPath)
			return nil
		},
	}
	return cmd
}

func resolveEvalFlows(spec string) ([]harness.Flow, error) {
	known := map[string]harness.Flow{
		"base":      harness.BaseFlow{},
		"stateless": harness.StatelessFlow{},
	}
	var flows []harness.Flow
	for _, name := range strings.Split(spec, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		flow, ok := known[name]
		if !ok {
			return nil, fmt.Errorf("unknown flow %q (known flows: base, stateless)", name)
		}
		flows = append(flows, flow)
	}
	if len(flows) == 0 {
		return nil, fmt.Errorf("no flows specified")
	}
	return flows, nil
}
