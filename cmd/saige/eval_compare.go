package main

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/urmzd/saige/eval"
	"github.com/urmzd/saige/eval/store"
)

// exitInconclusive is the exit status of an eval command that could not
// decide, such as a run lost to a provider outage or a comparison with too
// few comparable cases. CI can retry it instead of treating it as a failure
// (status 1).
const exitInconclusive = 3

// Report formats of saige eval compare.
const (
	formatText     = "text"
	formatJSON     = "json"
	formatMarkdown = "markdown"
	formatJUnit    = "junit"
)

// Defaults of saige eval compare.
const (
	defaultCompareSignificance = 0.05
	defaultCompareMinCases     = 3
	// maxListedCases caps the cases each report lists.
	maxListedCases = 50
)

// evalCompareFlags holds the flags of saige eval compare.
type evalCompareFlags struct {
	store, tenant, baseline, suite, output string
	baselineSuite                          string
	asserts, metrics, thresholds, lower    []string
	maxRegression, significance            float64
	maxInconclusive                        float64
	minCases                               int
}

func newEvalCompareCmd() *cobra.Command {
	var f evalCompareFlags
	cmd := &cobra.Command{
		Use:   "compare [candidate-run]",
		Short: "Gate a stored run against a baseline run: exit 1 on a regression",
		Long: `Compare a candidate run with a baseline run from the same results store,
case by case, and fail when a metric regressed.

The candidate is the run named as the argument, or the newest run of --suite.
The baseline is --baseline: a run ID, or "latest" (the default) for the newest
succeeded run of --baseline-suite (default the candidate's suite) that started
before the candidate, skipping runs whose gate was inconclusive. Record main
branch runs under their own suite and pass it as --baseline-suite, so a pull
request is never compared with another pull request.

For each metric the paired difference is computed over the cases both runs
measured: the pass rate (McNemar) for a metric both runs gated, the mean
(bootstrap) otherwise. A metric regresses when it drops by more than
--max-regression (or its --threshold), and, unless --significance is 1, when
the whole (1 - significance) interval of the drop lies beyond that tolerance.
Cases that one run could not measure because infrastructure failed are left
out, and a metric with fewer than --min-cases paired cases is inconclusive.
With --assert both runs are gated again first and the candidate's gate counts
too.

--format picks the report: human (text), markdown (for a pull request
comment), junit (for CI test reporters) or json. --output writes it to a
file; a one-line summary always goes to stderr.

Exit status: 0 no regression, 1 a metric regressed or the candidate failed
its gate, 3 inconclusive (too few comparable cases, a metric only one run
measured, or an inconclusive gate), 2 invalid input.`,
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEvalCompare(cmd, args, f)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.store, "store", "", storeFlagUsage+" (required)")
	_ = cmd.MarkFlagRequired("store")
	fl.StringVar(&f.tenant, "tenant", "", tenantFlagUsage)
	fl.StringVar(&f.baseline, "baseline", "latest", `Baseline run ID, or "latest" for the newest succeeded run of the candidate's suite before it`)
	fl.StringVar(&f.suite, "suite", "", "Suite whose newest run is the candidate when no run is given")
	fl.StringVar(&f.baselineSuite, "baseline-suite", "", `Suite the "latest" baseline comes from (default the candidate's suite)`)
	fl.StringArrayVar(&f.asserts, "assert", nil, `Gate both runs again on a metric, as in saige eval run ("turn_succeeded>=1", "aggregate:latency_ms<=2000"); repeatable`)
	fl.StringArrayVar(&f.metrics, "metric", nil, "Metric to check; repeatable (default: the metrics either run gated, or every shared metric when neither gated any)")
	fl.Float64Var(&f.maxRegression, "max-regression", 0, "Largest drop a metric may show and pass: a share of cases for a gated metric, the metric's units otherwise")
	fl.StringArrayVar(&f.thresholds, "threshold", nil, `Per-metric max regression, such as "faithfulness=0.05"; repeatable`)
	fl.Float64Var(&f.significance, "significance", defaultCompareSignificance, "Count a drop only when significant at this level; 1 counts any drop beyond the tolerance")
	fl.IntVar(&f.minCases, "min-cases", defaultCompareMinCases, "Fewest paired cases a metric needs; with fewer it is inconclusive")
	fl.StringArrayVar(&f.lower, "lower-is-better", nil, "Ungated metric where a rise is the regression, such as latency_ms; repeatable")
	fl.Float64Var(&f.maxInconclusive, "max-inconclusive", 0, "With --assert: largest share of inconclusive cases a passing gate tolerates, from 0 to 1")
	fl.StringVar(&f.output, "output", "", "Write the report to this file instead of stdout")
	return cmd
}

// compareFormat normalizes the global --format for saige eval compare.
func compareFormat(format string) (string, error) {
	switch format {
	case "", "human", formatText:
		return formatText, nil
	case formatJSON, formatMarkdown, formatJUnit:
		return format, nil
	case "md":
		return formatMarkdown, nil
	default:
		return "", fmt.Errorf("--format %q: saige eval compare writes human, markdown, junit or json", format)
	}
}

// comparePolicy builds the regression policy from the flags.
func comparePolicy(f evalCompareFlags) (eval.RegressionPolicy, float64, error) {
	p := eval.RegressionPolicy{
		Metrics:       f.metrics,
		MaxRegression: f.maxRegression,
		MinCases:      f.minCases,
		Significant:   f.significance < 1,
	}
	if f.significance <= 0 || f.significance > 1 {
		return p, 0, fmt.Errorf("--significance %g must be above 0 and at most 1", f.significance)
	}
	for _, spec := range f.thresholds {
		name, value, ok := strings.Cut(spec, "=")
		t, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if !ok || strings.TrimSpace(name) == "" || err != nil {
			return p, 0, fmt.Errorf(`--threshold %q: want metric=value, such as "faithfulness=0.05"`, spec)
		}
		if p.Thresholds == nil {
			p.Thresholds = map[string]float64{}
		}
		p.Thresholds[strings.TrimSpace(name)] = t
	}
	for _, m := range f.lower {
		if p.LowerIsBetter == nil {
			p.LowerIsBetter = map[string]bool{}
		}
		p.LowerIsBetter[m] = true
	}
	if p.MinCases < 1 {
		return p, 0, fmt.Errorf("--min-cases must be at least 1")
	}
	if err := p.Validate(); err != nil {
		return p, 0, err
	}
	confidence := 1 - f.significance
	if !p.Significant {
		confidence = eval.DefaultConfidence
	}
	return p, confidence, nil
}

// runRef identifies a compared run in a report.
type runRef struct {
	ID        string         `json:"id"`
	Suite     string         `json:"suite"`
	Status    eval.RunStatus `json:"status"`
	Outcome   eval.Outcome   `json:"outcome,omitempty"`
	Commit    string         `json:"commit,omitempty"`
	StartedAt time.Time      `json:"started_at"`
	Units     int            `json:"units"`
}

func refOf(r eval.RunRecord) runRef {
	ref := runRef{ID: r.ID, Suite: r.Suite, Status: r.Status, Outcome: r.Outcome, StartedAt: r.StartedAt, Units: r.Units}
	if r.Provenance.GitCommit != "" {
		ref.Commit = formatCommit(r.Provenance)
	}
	return ref
}

// compareGate is the candidate's own gate when --assert re-gated the runs.
type compareGate struct {
	Outcome    eval.Outcome     `json:"outcome"`
	Violations []eval.Violation `json:"violations,omitempty"`
}

// compareReport is what saige eval compare prints.
type compareReport struct {
	eval.RegressionReport
	Baseline  runRef       `json:"baseline"`
	Candidate runRef       `json:"candidate"`
	Gate      *compareGate `json:"gate,omitempty"`
}

func runEvalCompare(cmd *cobra.Command, args []string, f evalCompareFlags) error {
	ctx := cmd.Context()
	format, err := compareFormat(*persistentFlagVars.format)
	if err != nil {
		return invalidInput(err)
	}
	policy, confidence, err := comparePolicy(f)
	if err != nil {
		return invalidInput(err)
	}
	gatePolicy := eval.GatePolicy{MaxInconclusive: f.maxInconclusive}
	if err := gatePolicy.Validate(); err != nil {
		return invalidInput(fmt.Errorf("--max-inconclusive: %w", err))
	}
	var asserts []eval.Assertion
	for _, spec := range f.asserts {
		a, err := parseEvalAssertion(spec)
		if err != nil {
			return invalidInput(err)
		}
		asserts = append(asserts, a)
	}
	if len(args) == 0 && f.suite == "" {
		return invalidInput(errors.New("name the candidate run, or pass --suite to compare its newest run"))
	}

	s, closeStore, err := openEvalStore(ctx, f.store, f.tenant, false)
	if err != nil {
		return err
	}
	defer closeStore()

	var cand eval.RunRecord
	if len(args) == 1 {
		if cand, err = s.GetRun(ctx, args[0]); err != nil {
			return err
		}
	} else {
		runs, err := s.ListRuns(ctx, store.RunFilter{Suite: f.suite, Limit: 1})
		if err != nil {
			return err
		}
		if len(runs) == 0 {
			return fmt.Errorf("%w: suite %q has no runs", store.ErrNotFound, f.suite)
		}
		cand = runs[0]
	}
	var base eval.RunRecord
	if f.baseline == "latest" {
		base, err = store.BaselineFor(ctx, s, f.baselineSuite, cand)
	} else {
		base, err = s.GetRun(ctx, f.baseline)
	}
	if err != nil {
		return fmt.Errorf("baseline: %w", err)
	}
	if base.ID == cand.ID {
		return invalidInput(fmt.Errorf("the baseline and the candidate are the same run %q", cand.ID))
	}

	baseSuite, err := store.LoadSuite(ctx, s, base.ID)
	if err != nil {
		return err
	}
	candSuite, err := store.LoadSuite(ctx, s, cand.ID)
	if err != nil {
		return err
	}
	rep := compareReport{Baseline: refOf(base), Candidate: refOf(cand)}
	if len(asserts) > 0 {
		baseSuite.GateWith(gatePolicy, asserts...)
		candSuite.GateWith(gatePolicy, asserts...)
		rep.Gate = &compareGate{Outcome: candSuite.Outcome, Violations: candSuite.Violations}
	}
	opts := []eval.Option{
		eval.WithName(cand.Suite),
		eval.WithBootstrap(eval.DefaultResamples, confidence),
		eval.WithProvenance(base.Provenance, cand.Provenance),
	}
	for m := range policy.LowerIsBetter {
		opts = append(opts, eval.WithLowerIsBetter(m))
	}
	rep.RegressionReport = eval.CompareSuites(baseSuite, candSuite, opts...).Gate(policy)
	rep.Base, rep.Exp = base.ID, cand.ID
	if base.Suite != cand.Suite && f.baselineSuite != base.Suite {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("the baseline is from suite %q and the candidate from %q", base.Suite, cand.Suite))
	}
	if cand.Status != eval.RunSucceeded {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("the candidate run is %s", cand.Status))
	}
	if rep.Gate != nil {
		switch rep.Gate.Outcome {
		case eval.OutcomeFailed:
			rep.Outcome = eval.OutcomeFailed
		case eval.OutcomeInconclusive:
			if rep.Outcome == eval.OutcomePassed {
				rep.Outcome = eval.OutcomeInconclusive
			}
		}
	}

	if err := emitCompareReport(cmd.OutOrStdout(), f.output, format, rep); err != nil {
		return err
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "compare: %s\n", compareSummary(rep))

	switch rep.Outcome {
	case eval.OutcomeFailed:
		return reportedError{exitError{code: 1, err: errors.New("regression: " + compareSummary(rep))}}
	case eval.OutcomeInconclusive:
		return reportedError{exitError{code: exitInconclusive, err: errors.New("inconclusive: " + compareSummary(rep))}}
	}
	return nil
}

// emitCompareReport writes the report to path, or to stdout when path is
// empty.
func emitCompareReport(stdout io.Writer, path, format string, rep compareReport) error {
	if path == "" {
		return writeCompareReport(stdout, format, rep)
	}
	file, err := os.Create(filepath.Clean(path))
	if err != nil {
		return err
	}
	if err := writeCompareReport(file, format, rep); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// compareSummary is the one-line result of a comparison.
func compareSummary(rep compareReport) string {
	regressed, inconclusive := 0, 0
	for _, m := range rep.Metrics {
		switch m.Status {
		case eval.MetricRegressed:
			regressed++
		case eval.MetricInconclusive:
			inconclusive++
		}
	}
	parts := []string{fmt.Sprintf("%d of %d metrics regressed", regressed, len(rep.Metrics))}
	if inconclusive > 0 {
		parts = append(parts, fmt.Sprintf("%d inconclusive", inconclusive))
	}
	if rep.Gate != nil {
		parts = append(parts, "candidate gate "+string(rep.Gate.Outcome))
	}
	return fmt.Sprintf("%s (%s), %s vs baseline %s", rep.Outcome, strings.Join(parts, ", "), rep.Candidate.ID, rep.Baseline.ID)
}

func writeCompareReport(w io.Writer, format string, rep compareReport) error {
	switch format {
	case formatJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	case formatMarkdown:
		return writeCompareMarkdown(w, rep)
	case formatJUnit:
		return writeCompareJUnit(w, rep)
	default:
		return writeCompareText(w, rep)
	}
}

// formatValue prints a metric value compactly.
func formatValue(v float64) string { return strconv.FormatFloat(v, 'g', 4, 64) }

func formatDelta(v float64) string {
	if v >= 0 {
		return "+" + formatValue(v)
	}
	return formatValue(v)
}

func formatInterval(m eval.MetricVerdict) string {
	if m.Basis == "" {
		return "-"
	}
	return "[" + formatDelta(m.Low) + ", " + formatDelta(m.High) + "]"
}

// verdictCells returns the base, candidate, delta and interval cells of a
// metric, or dashes when it was not compared.
func verdictCells(m eval.MetricVerdict) (base, exp, delta, interval, n string) {
	if m.Basis == "" {
		return "-", "-", "-", "-", "-"
	}
	return formatValue(m.Base), formatValue(m.Exp), formatDelta(m.Delta), formatInterval(m), strconv.Itoa(m.N)
}

func basisName(m eval.MetricVerdict) string {
	switch {
	case m.Basis == eval.BasisPassRate:
		return "pass rate"
	case m.Basis == eval.BasisMean && m.LowerIsBetter:
		return "mean, lower is better"
	case m.Basis == eval.BasisMean:
		return "mean"
	default:
		return "-"
	}
}

// confidenceLabel names the interval level, such as "95%".
func confidenceLabel(c float64) string {
	if c <= 0 {
		c = eval.DefaultConfidence
	}
	return strconv.FormatFloat(100*c, 'g', 4, 64) + "%"
}

func writeCompareText(w io.Writer, rep compareReport) error {
	fmt.Fprintf(w, "outcome    %s\n", rep.Outcome)
	fmt.Fprintf(w, "baseline   %s\n", describeRun(rep.Baseline))
	fmt.Fprintf(w, "candidate  %s\n", describeRun(rep.Candidate))
	if rep.BaseInconclusive+rep.ExpInconclusive > 0 {
		fmt.Fprintf(w, "unmeasured %d baseline and %d candidate results (infrastructure failed)\n", rep.BaseInconclusive, rep.ExpInconclusive)
	}
	for _, warning := range rep.Warnings {
		fmt.Fprintf(w, "warning    %s\n", warning)
	}
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "METRIC\tBASIS\tBASELINE\tCANDIDATE\tDELTA\t%s INTERVAL\tN\tSTATUS\n", confidenceLabel(rep.Confidence))
	for _, m := range rep.Metrics {
		base, exp, delta, interval, n := verdictCells(m)
		status := string(m.Status)
		if m.Reason != "" {
			status += ": " + m.Reason
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", m.Metric, basisName(m), base, exp, delta, interval, n, status)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if rep.Gate != nil {
		fmt.Fprintf(w, "\ncandidate gate: %s\n", rep.Gate.Outcome)
		for _, v := range rep.Gate.Violations {
			fmt.Fprintf(w, "  %s\n", v)
		}
	}
	writeTextCases(w, "regressed cases", rep.Regressions)
	writeTextCases(w, "inconclusive cases", rep.Inconclusive)
	return nil
}

func describeRun(r runRef) string {
	parts := []string{r.Suite, string(r.Status)}
	if r.Outcome != "" {
		parts = append(parts, "gate "+string(r.Outcome))
	}
	if r.Commit != "" {
		parts = append(parts, "commit "+r.Commit)
	}
	parts = append(parts, fmt.Sprintf("%d units", r.Units))
	return r.ID + " (" + strings.Join(parts, ", ") + ")"
}

func caseName(d eval.CaseDiff) string {
	name := d.ID
	if d.Turn != 0 {
		name += "/" + strconv.Itoa(d.Turn)
	}
	if d.Variant != "" {
		name += "@" + d.Variant
	}
	return name
}

// caseCells returns the baseline and candidate cells of a case: the pass
// rate when gated, the value otherwise, and "unmeasured" for a side an
// inconclusive case is missing.
func caseCells(d eval.CaseDiff) (string, string) {
	cell := func(rate, value *float64) string {
		switch {
		case rate != nil:
			return "pass " + formatValue(*rate)
		case value != nil:
			return formatValue(*value)
		case d.Status == eval.CaseInconclusive:
			return "unmeasured"
		default:
			return "-"
		}
	}
	return cell(d.BasePassRate, d.Base), cell(d.ExpPassRate, d.Exp)
}

func writeTextCases(w io.Writer, title string, cases []eval.CaseDiff) {
	if len(cases) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s (%d):\n", title, len(cases))
	for i, d := range cases {
		if i == maxListedCases {
			fmt.Fprintf(w, "  and %d more\n", len(cases)-i)
			break
		}
		b, e := caseCells(d)
		fmt.Fprintf(w, "  %s %s: %s -> %s\n", caseName(d), d.Metric, b, e)
	}
}

// markdownCell escapes the characters that would break a table cell.
func markdownCell(s string) string {
	return strings.NewReplacer("|", `\|`, "\n", " ").Replace(s)
}

func writeCompareMarkdown(w io.Writer, rep compareReport) error {
	regressed := 0
	for _, m := range rep.Metrics {
		if m.Status == eval.MetricRegressed {
			regressed++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "### saige eval compare: %s\n\n", rep.Outcome)
	fmt.Fprintf(&b, "**%s**: %d of %d metrics regressed. Candidate `%s` against baseline `%s`", rep.Outcome, regressed, len(rep.Metrics), rep.Candidate.ID, rep.Baseline.ID)
	if rep.Candidate.Commit != "" && rep.Baseline.Commit != "" {
		fmt.Fprintf(&b, " (commit `%s` against `%s`)", rep.Candidate.Commit, rep.Baseline.Commit)
	}
	fmt.Fprintf(&b, ", suite `%s`.\n\n", rep.Candidate.Suite)
	if rep.BaseInconclusive+rep.ExpInconclusive > 0 {
		fmt.Fprintf(&b, "%d baseline and %d candidate results could not be measured because infrastructure failed; they are left out.\n\n", rep.BaseInconclusive, rep.ExpInconclusive)
	}
	for _, warning := range rep.Warnings {
		fmt.Fprintf(&b, "> Warning: %s\n\n", markdownCell(warning))
	}
	fmt.Fprintf(&b, "| Metric | Basis | Baseline | Candidate | Delta | %s interval | Cases | Status |\n", confidenceLabel(rep.Confidence))
	b.WriteString("| --- | --- | ---: | ---: | ---: | --- | ---: | --- |\n")
	for _, m := range rep.Metrics {
		base, exp, delta, interval, n := verdictCells(m)
		status := "**" + string(m.Status) + "**"
		if m.Status == eval.MetricPassed {
			status = string(m.Status)
		}
		if m.Reason != "" {
			status += ": " + markdownCell(m.Reason)
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s | %s | %s | %s |\n", markdownCell(m.Metric), basisName(m), base, exp, delta, interval, n, status)
	}
	if rep.Gate != nil {
		fmt.Fprintf(&b, "\nCandidate gate: **%s**.\n", rep.Gate.Outcome)
		if len(rep.Gate.Violations) > 0 {
			fmt.Fprintf(&b, "\n<details><summary>Gate violations (%d)</summary>\n\n", len(rep.Gate.Violations))
			for i, v := range rep.Gate.Violations {
				if i == maxListedCases {
					fmt.Fprintf(&b, "- and %d more\n", len(rep.Gate.Violations)-i)
					break
				}
				fmt.Fprintf(&b, "- %s\n", markdownCell(v.String()))
			}
			b.WriteString("\n</details>\n")
		}
	}
	writeMarkdownCases(&b, "Regressed cases", rep.Regressions)
	writeMarkdownCases(&b, "Inconclusive cases", rep.Inconclusive)
	_, err := io.WriteString(w, b.String())
	return err
}

func writeMarkdownCases(b *strings.Builder, title string, cases []eval.CaseDiff) {
	if len(cases) == 0 {
		return
	}
	fmt.Fprintf(b, "\n<details><summary>%s (%d)</summary>\n\n", title, len(cases))
	b.WriteString("| Case | Metric | Baseline | Candidate |\n| --- | --- | ---: | ---: |\n")
	for i, d := range cases {
		if i == maxListedCases {
			fmt.Fprintf(b, "\nand %d more\n", len(cases)-i)
			break
		}
		base, exp := caseCells(d)
		fmt.Fprintf(b, "| `%s` | `%s` | %s | %s |\n", markdownCell(caseName(d)), markdownCell(d.Metric), base, exp)
	}
	b.WriteString("\n</details>\n")
}

// JUnit XML: one test case per checked metric, plus one for the candidate's
// gate when --assert re-gated the runs. A regression is a failure and an
// inconclusive verdict is skipped, so CI reporters show both without custom
// parsing.
type junitSuites struct {
	XMLName  xml.Name     `xml:"testsuites"`
	Name     string       `xml:"name,attr"`
	Tests    int          `xml:"tests,attr"`
	Failures int          `xml:"failures,attr"`
	Skipped  int          `xml:"skipped,attr"`
	Suites   []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name       string          `xml:"name,attr"`
	Tests      int             `xml:"tests,attr"`
	Failures   int             `xml:"failures,attr"`
	Skipped    int             `xml:"skipped,attr"`
	Properties []junitProperty `xml:"properties>property,omitempty"`
	Cases      []junitCase     `xml:"testcase"`
}

type junitProperty struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	ClassName string        `xml:"classname,attr"`
	Failure   *junitMessage `xml:"failure,omitempty"`
	Skipped   *junitMessage `xml:"skipped,omitempty"`
	SystemOut string        `xml:"system-out,omitempty"`
}

type junitMessage struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr,omitempty"`
	Body    string `xml:",cdata"`
}

func writeCompareJUnit(w io.Writer, rep compareReport) error {
	class := "saige.eval.compare." + rep.Candidate.Suite
	suite := junitSuite{
		Name: rep.Candidate.Suite,
		Properties: []junitProperty{
			{Name: "baseline", Value: rep.Baseline.ID},
			{Name: "candidate", Value: rep.Candidate.ID},
			{Name: "outcome", Value: string(rep.Outcome)},
		},
	}
	for _, m := range rep.Metrics {
		base, exp, delta, interval, n := verdictCells(m)
		c := junitCase{
			Name:      m.Metric,
			ClassName: class,
			SystemOut: fmt.Sprintf("%s: baseline %s, candidate %s, delta %s, %s interval %s, %s cases", basisName(m), base, exp, delta, confidenceLabel(rep.Confidence), interval, n),
		}
		switch m.Status {
		case eval.MetricRegressed:
			var body strings.Builder
			for _, d := range rep.Regressions {
				if d.Metric == m.Metric {
					b, e := caseCells(d)
					fmt.Fprintf(&body, "%s: %s -> %s\n", caseName(d), b, e)
				}
			}
			c.Failure = &junitMessage{Message: m.Reason, Type: "regression", Body: body.String()}
			suite.Failures++
		case eval.MetricInconclusive:
			c.Skipped = &junitMessage{Message: "inconclusive: " + m.Reason}
			suite.Skipped++
		}
		suite.Cases = append(suite.Cases, c)
	}
	if rep.Gate != nil {
		c := junitCase{Name: "gate", ClassName: class}
		var body strings.Builder
		for _, v := range rep.Gate.Violations {
			body.WriteString(v.String() + "\n")
		}
		switch rep.Gate.Outcome {
		case eval.OutcomeFailed:
			c.Failure = &junitMessage{Message: "the candidate failed its gate", Type: "gate", Body: body.String()}
			suite.Failures++
		case eval.OutcomeInconclusive:
			c.Skipped = &junitMessage{Message: "inconclusive: " + strings.TrimSpace(body.String())}
			suite.Skipped++
		}
		suite.Cases = append(suite.Cases, c)
	}
	if len(suite.Cases) == 0 {
		suite.Cases = append(suite.Cases, junitCase{Name: "metrics", ClassName: class, Skipped: &junitMessage{Message: "inconclusive: no metric to compare"}})
		suite.Skipped++
	}
	suite.Tests = len(suite.Cases)
	doc := junitSuites{Name: "saige eval compare", Tests: suite.Tests, Failures: suite.Failures, Skipped: suite.Skipped, Suites: []junitSuite{suite}}
	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}
