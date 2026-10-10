package eval

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/urmzd/saige/eval/analysis"
)

// Defaults for the intervals behind a [Comparison].
const (
	DefaultResamples  = 2000
	DefaultConfidence = analysis.DefaultConfidence
	defaultSeed       = 42
)

// Stat is one estimate with its interval; see [analysis.Stat].
type Stat = analysis.Stat

// DeltaStats is the paired difference (exp minus base) for one metric. Only
// observations scored successfully on both sides, matched by case (see
// [CaseDiff]), are counted. Repeated samples of a case are averaged first,
// so each case is one pair and N is the number of cases, not samples. Delta
// is the mean paired difference and [CILow, CIHigh] its bootstrap
// percentile interval; an interval that contains 0 means the comparison did
// not separate the arms. With N below 2 the interval collapses to Delta and
// carries no information.
type DeltaStats struct {
	Delta  float64 `json:"delta"`
	CILow  float64 `json:"ci_low"`
	CIHigh float64 `json:"ci_high"`
	N      int     `json:"n"`
}

// PassStats compares pass/fail verdicts ([Score.Passed]) of one metric on
// the same cases, matched as for [DeltaStats]. When every case has one
// verdict per arm, Base and Exp are each arm's pass rate over the pairs with
// a Wilson interval, Diff is exp minus base with McNemar's paired interval,
// and McNemarP the exact two-sided p-value; both are driven only by the
// cases where the arms disagree.
//
// When some case was sampled more than once, CaseRates is set: Base, Exp,
// and Diff are means of per-case pass rates with case-level bootstrap
// intervals, N counts cases, Pairs is empty, and McNemarP is 1 because
// McNemar's test needs one verdict per case.
type PassStats struct {
	Pairs     analysis.Paired `json:"pairs"`
	Base      Stat            `json:"base"`
	Exp       Stat            `json:"exp"`
	Diff      Stat            `json:"diff"`
	McNemarP  float64         `json:"mcnemar_p"`
	CaseRates bool            `json:"case_rates,omitempty"`
}

// CaseStatus classifies how one case changed between arms.
type CaseStatus string

// Case statuses reported in [Comparison.Cases].
const (
	CaseImproved  CaseStatus = "improved"
	CaseRegressed CaseStatus = "regressed"
	CaseUnchanged CaseStatus = "unchanged"
	// CaseOnlyBase means only the base arm scored the case, as when the exp
	// subject or scorer failed on it.
	CaseOnlyBase CaseStatus = "only_base"
	// CaseOnlyExp means only the exp arm scored the case.
	CaseOnlyExp CaseStatus = "only_exp"
	// CaseInconclusive means the arms cannot be compared on the case
	// because one of them could not measure it: its subject or scorer
	// failed on infrastructure (see [IsInfra]).
	CaseInconclusive CaseStatus = "inconclusive"
)

// CaseDiff is one case's change in one metric.
//
// A case is identified by ID, Turn, and the [LabelVariant] label, so two
// suites from [Experiment.Run] pair each variant with itself. When each arm
// holds a single variant and they differ, as in [CompareVariants], the
// variant is left out of the key so the arms pair by ID and Turn.
//
// Samples of the same case (same ID, Turn, and Variant) are rolled up: Base and Exp are the mean of the
// successful values, and BasePassRate and ExpPassRate the share of gated
// samples that passed. When both arms were gated the status follows the
// pass rates; otherwise it follows the values, where higher is better unless
// the metric was marked with [WithLowerIsBetter].
type CaseDiff struct {
	ID   string `json:"id"`
	Turn int    `json:"turn,omitempty"`
	// Variant is the case's [LabelVariant] label when variants are part
	// of the key.
	Variant      string     `json:"variant,omitempty"`
	Metric       string     `json:"metric"`
	Base         *float64   `json:"base,omitempty"`
	Exp          *float64   `json:"exp,omitempty"`
	BasePassRate *float64   `json:"base_pass_rate,omitempty"`
	ExpPassRate  *float64   `json:"exp_pass_rate,omitempty"`
	Change       float64    `json:"change"`
	Status       CaseStatus `json:"status"`
}

// Comparison is the result of comparing a candidate arm (exp) against a
// base arm on the same cases.
type Comparison struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	// Base and Exp name the compared arms when they are known, such as
	// the variant names given to [CompareVariants].
	Base          string              `json:"base,omitempty"`
	Exp           string              `json:"exp,omitempty"`
	BaseResults   []ObservationResult `json:"base_results"`
	ExpResults    []ObservationResult `json:"exp_results"`
	BaseAggregate map[string]float64  `json:"base_aggregate"`
	ExpAggregate  map[string]float64  `json:"exp_aggregate"`
	// Deltas is the exp aggregate minus the base aggregate, for metrics
	// present on both sides. See DeltaStats for the paired comparison.
	Deltas map[string]float64 `json:"deltas"`
	// DeltaStats holds the paired difference and its confidence interval
	// per metric.
	DeltaStats map[string]DeltaStats `json:"delta_stats,omitempty"`
	// PassStats holds the paired pass-rate comparison per metric, for
	// metrics gated on both sides (see [WithAssertions]).
	PassStats map[string]PassStats `json:"pass_stats,omitempty"`
	// Cases is the per-case diff, sorted by metric, ID, Turn, and Variant. See
	// [Comparison.Regressions].
	Cases []CaseDiff `json:"cases,omitempty"`
	// BaseCounts and ExpCounts are the number of successful scores behind
	// each aggregate.
	BaseCounts map[string]int `json:"base_counts,omitempty"`
	ExpCounts  map[string]int `json:"exp_counts,omitempty"`
	// MissingInBase lists metrics the exp arm scored but the base arm never
	// did, and MissingInExp the reverse. Such metrics have no delta.
	MissingInBase []string `json:"missing_in_base,omitempty"`
	MissingInExp  []string `json:"missing_in_exp,omitempty"`
	// BaseErroredCases and ExpErroredCases count observations with at least
	// one errored score on each side; errored scores are excluded from the
	// aggregates and deltas.
	BaseErroredCases int `json:"base_errored_cases,omitempty"`
	ExpErroredCases  int `json:"exp_errored_cases,omitempty"`
	// BaseSubjectErrors and ExpSubjectErrors count observations whose
	// subject failed; see [AnnotationSubjectError].
	BaseSubjectErrors int `json:"base_subject_errors,omitempty"`
	ExpSubjectErrors  int `json:"exp_subject_errors,omitempty"`
	// BaseIncomplete and ExpIncomplete count observations left unscored
	// because the context ended.
	BaseIncomplete int `json:"base_incomplete,omitempty"`
	ExpIncomplete  int `json:"exp_incomplete,omitempty"`
	// BaseInconclusive and ExpInconclusive count results that could not
	// be measured because infrastructure failed; they are left out of the
	// aggregates, deltas, and pass rates.
	BaseInconclusive int `json:"base_inconclusive,omitempty"`
	ExpInconclusive  int `json:"exp_inconclusive,omitempty"`
	// Confidence is the confidence level of the intervals in DeltaStats and
	// PassStats.
	Confidence float64 `json:"confidence,omitempty"`
	// BaseOutcome and ExpOutcome are each arm's gate outcome, when gated.
	// [CompareVariants] sets them only when given [WithAssertions].
	BaseOutcome Outcome `json:"base_outcome,omitempty"`
	ExpOutcome  Outcome `json:"exp_outcome,omitempty"`
	// Warnings report why the arms may not be comparable, such as a profile
	// that served with a different configuration hash. See WithProvenance.
	Warnings []string `json:"warnings,omitempty"`
}

// ExperimentResult is the former name of [Comparison].
//
// Deprecated: Use [Comparison].
type ExperimentResult = Comparison

// Regressions returns the cases whose status is [CaseRegressed].
func (c *Comparison) Regressions() []CaseDiff {
	var out []CaseDiff
	for _, d := range c.Cases {
		if d.Status == CaseRegressed {
			out = append(out, d)
		}
	}
	return out
}

// Compare runs both subjects on the same inputs, scores them, and compares
// the exp arm against the base arm.
//
// Options apply as in [Run] and [PopulateAll]: [WithConcurrency] caps how
// many subjects and scorers run at once, [WithSampler] samples the scorers,
// [WithRepeats] runs each subject several times per input, and
// [WithAssertions] gates both arms so [Comparison.PassStats] can compare
// their verdicts. A subject error on one observation is recorded on it and
// the rest continue. When scoring returns an error, for example because ctx
// ended, Compare returns the partial result with that error instead of
// discarding the work already done.
func Compare(ctx context.Context, inputs []Observation, base, exp Subject, scorers []Scorer, opts ...Option) (*Comparison, error) {
	cfg := newConfig(append([]Option{WithName("comparison")}, opts...))

	inputs = Sampler{N: cfg.Repeats}.Replicate(inputs)
	// Deep-copy inputs for each subject so they don't interfere.
	baseObs := copyObservations(inputs)
	expObs := copyObservations(inputs)

	// Subject failures are recorded per observation and reported through
	// the subject error counts, so they do not stop the comparison.
	var baseSuite, expSuite *SuiteResult
	var baseErr, expErr error
	if cfg.batch != nil {
		baseSuite, expSuite, baseErr, expErr = compareBatched(ctx, cfg, baseObs, expObs, base, exp, scorers)
	} else {
		_ = PopulateAll(ctx, baseObs, base, opts...)
		_ = PopulateAll(ctx, expObs, exp, opts...)
		baseSuite, baseErr = Run(ctx, cfg.Name+"/base", baseObs, scorers, opts...)
		expSuite, expErr = Run(ctx, cfg.Name+"/exp", expObs, scorers, opts...)
	}

	result := compareSuites(cfg, baseSuite, expSuite)

	if cfg.OutputDir != "" {
		if err := WriteComparison(cfg.OutputDir, result); err != nil {
			cfg.Logger.Error("failed to write comparison", "dir", cfg.OutputDir, "error", err)
		}
	}

	return result, errors.Join(baseErr, expErr)
}

// RunExperiment runs both subjects on the same inputs and compares them.
//
// Deprecated: Use [Compare], which takes the same arguments.
func RunExperiment(ctx context.Context, inputs []Observation, base, exp Subject, scorers []Scorer, opts ...ExperimentOption) (*ExperimentResult, error) {
	return Compare(ctx, inputs, base, exp, scorers, append([]Option{WithName("experiment")}, opts...)...)
}

// CompareSuites compares two finished suites, such as a baseline saved with
// [WriteSuiteResult] and the current run, case by case. It runs nothing.
// Gate both suites with the same assertions first to get
// [Comparison.PassStats] and pass-based case statuses.
func CompareSuites(base, exp *SuiteResult, opts ...Option) *Comparison {
	cfg := newConfig(opts)
	if cfg.Name == "" {
		cfg.Name = exp.Name
	}
	c := compareSuites(cfg, base, exp)
	c.Base, c.Exp = base.Name, exp.Name
	if cfg.provenance != nil {
		c.Warnings = ConfigDrift(cfg.provenance[0], cfg.provenance[1])
	}
	return c
}

// CompareVariants compares every variant in results against the baseline
// variant, pairing cases by ID, Turn, and Sample. Variants are read from the
// [LabelVariant] label that [Experiment.Run] sets. It returns one
// [Comparison] per candidate variant, keyed by its name, and an error when
// no result carries the baseline variant.
//
// Pass the experiment's assertions with [WithAssertions] to get BaseOutcome
// and ExpOutcome: each variant is gated on its own with the assertions that
// apply to it, those with no [LabelVariant] in their Where or with that
// variant's name there, and those verdicts drive [Comparison.PassStats].
// Without assertions the outcomes are left empty and PassStats uses the
// [Score.Passed] verdicts already in results. The results are not modified.
func CompareVariants(results []ObservationResult, baseline string, opts ...Option) (map[string]*Comparison, error) {
	cfg := newConfig(opts)
	byVariant := map[string]*SuiteResult{}
	var order []string
	for _, r := range results {
		name := r.Observation.Labels[LabelVariant]
		s, ok := byVariant[name]
		if !ok {
			s = &SuiteResult{Name: name}
			byVariant[name] = s
			order = append(order, name)
		}
		s.Results = append(s.Results, r)
	}
	base, ok := byVariant[baseline]
	if !ok {
		return nil, fmt.Errorf("compare variants: no results for baseline variant %q", baseline)
	}
	summarize(base, baseline, cfg.Assertions)

	out := make(map[string]*Comparison, len(byVariant)-1)
	for _, name := range order {
		if name == baseline {
			continue
		}
		exp := byVariant[name]
		summarize(exp, name, cfg.Assertions)
		vcfg := *cfg
		if vcfg.Name == "" {
			vcfg.Name = baseline + " vs " + name
		}
		c := compareSuites(&vcfg, base, exp)
		c.Base, c.Exp = baseline, name
		out[name] = c
	}
	return out, nil
}

// summarize fills the counters of a suite assembled from the results of one
// variant and, given assertions, gates it with the ones that apply to that
// variant. Gating works on a copy of the scores so the caller's results keep
// their verdicts.
func summarize(s *SuiteResult, variant string, assertions []Assertion) {
	s.Aggregate = Aggregate(s.Results)
	s.Inconclusive = countInconclusive(s.Results)
	for _, r := range s.Results {
		if SubjectError(r.Observation) != "" {
			s.SubjectErrors++
			continue
		}
		for _, sc := range r.Scores {
			if sc.Error != "" {
				s.ErroredCases++
				break
			}
		}
	}
	if len(assertions) == 0 {
		return
	}
	var applicable []Assertion
	for _, a := range assertions {
		if v, ok := a.Where[LabelVariant]; !ok || v == variant {
			applicable = append(applicable, a)
		}
	}
	for i := range s.Results {
		s.Results[i].Scores = slices.Clone(s.Results[i].Scores)
	}
	s.Gate(applicable...)
}

func compareSuites(cfg *Config, baseSuite, expSuite *SuiteResult) *Comparison {
	baseAgg, baseCounts := AggregateCounts(baseSuite.Results)
	expAgg, expCounts := AggregateCounts(expSuite.Results)

	result := &Comparison{
		Name:              cfg.Name,
		CreatedAt:         time.Now(),
		BaseResults:       baseSuite.Results,
		ExpResults:        expSuite.Results,
		BaseAggregate:     baseAgg,
		ExpAggregate:      expAgg,
		Deltas:            make(map[string]float64),
		DeltaStats:        make(map[string]DeltaStats),
		BaseCounts:        baseCounts,
		ExpCounts:         expCounts,
		BaseErroredCases:  baseSuite.ErroredCases,
		ExpErroredCases:   expSuite.ErroredCases,
		BaseSubjectErrors: baseSuite.SubjectErrors,
		ExpSubjectErrors:  expSuite.SubjectErrors,
		BaseIncomplete:    baseSuite.Incomplete,
		ExpIncomplete:     expSuite.Incomplete,
		BaseInconclusive:  baseSuite.Inconclusive,
		ExpInconclusive:   expSuite.Inconclusive,
		BaseOutcome:       baseSuite.Outcome,
		ExpOutcome:        expSuite.Outcome,
	}

	key := caseKeyer(baseSuite.Results, expSuite.Results)
	resamples := cfg.Resamples
	if resamples <= 0 {
		resamples = DefaultResamples
	}
	confidence := cfg.Confidence
	if confidence <= 0 || confidence >= 1 {
		confidence = DefaultConfidence
	}
	result.Confidence = confidence

	for name, expVal := range expAgg {
		baseVal, ok := baseAgg[name]
		if !ok {
			result.MissingInBase = append(result.MissingInBase, name)
			continue
		}
		result.Deltas[name] = expVal - baseVal
		if stats, ok := pairedDelta(baseSuite.Results, expSuite.Results, key, name, resamples, confidence); ok {
			result.DeltaStats[name] = stats
		}
	}
	for name := range baseAgg {
		if _, ok := expAgg[name]; !ok {
			result.MissingInExp = append(result.MissingInExp, name)
		}
	}
	sort.Strings(result.MissingInBase)
	sort.Strings(result.MissingInExp)

	result.PassStats = pairedPasses(baseSuite.Results, expSuite.Results, key, resamples, confidence)
	result.Cases = caseDiffs(baseSuite.Results, expSuite.Results, key, cfg.LowerIsBetter)
	return result
}

// pairKey matches the same case across the two arms.
type pairKey struct {
	id      string
	turn    int
	sample  int
	variant string
}

// keyer maps an observation to its case key.
type keyer func(Observation) pairKey

func keyWithVariant(obs Observation) pairKey {
	return pairKey{obs.ID, obs.Turn, obs.Sample, obs.Labels[LabelVariant]}
}

func keyWithoutVariant(obs Observation) pairKey {
	return pairKey{obs.ID, obs.Turn, obs.Sample, ""}
}

// caseKeyer picks how cases pair across two arms. The variant is part of
// the key unless each arm holds exactly one variant and the two differ, the
// shape of a variant-against-control comparison.
func caseKeyer(base, exp []ObservationResult) keyer {
	b, bOK := singleVariant(base)
	e, eOK := singleVariant(exp)
	if bOK && eOK && b != e {
		return keyWithoutVariant
	}
	return keyWithVariant
}

// singleVariant returns the one [LabelVariant] value all results share.
func singleVariant(results []ObservationResult) (string, bool) {
	if len(results) == 0 {
		return "", false
	}
	v := results[0].Observation.Labels[LabelVariant]
	for _, r := range results[1:] {
		if r.Observation.Labels[LabelVariant] != v {
			return "", false
		}
	}
	return v, true
}

func (a pairKey) less(b pairKey) bool {
	if a.id != b.id {
		return a.id < b.id
	}
	if a.turn != b.turn {
		return a.turn < b.turn
	}
	if a.sample != b.sample {
		return a.sample < b.sample
	}
	return a.variant < b.variant
}

// caseOf drops the sample from a pair key. Repeats of one case are
// independent draws, so sample i of one arm has nothing to do with sample i
// of the other: samples are averaged within their case, and cases, not
// samples, are the paired units.
func caseOf(k pairKey) pairKey {
	k.sample = 0
	return k
}

// sortedCases returns the cases present in both maps, sorted so seeded
// resampling does not depend on map order.
func sortedCases[V any](base, exp map[pairKey]V) []pairKey {
	keys := make([]pairKey, 0, len(exp))
	for k := range exp {
		if _, ok := base[k]; ok {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].less(keys[j]) })
	return keys
}

// successfulScores indexes a metric's successful values by case, one value
// per sample.
func successfulScores(results []ObservationResult, key keyer, metric string) map[pairKey][]float64 {
	out := make(map[pairKey][]float64)
	for _, r := range results {
		for _, s := range r.Scores {
			if s.Name == metric && s.Error == "" {
				k := caseOf(key(r.Observation))
				out[k] = append(out[k], s.Value)
			}
		}
	}
	return out
}

// pairedDelta computes the mean paired difference for one metric and a
// percentile bootstrap interval over the paired differences. Each case
// contributes one difference, between the means of its samples on each arm,
// so N counts cases however many repeats ran. The resampling uses a fixed
// seed so the same results always give the same interval.
func pairedDelta(base, exp []ObservationResult, key keyer, metric string, resamples int, confidence float64) (DeltaStats, bool) {
	baseVals := successfulScores(base, key, metric)
	expVals := successfulScores(exp, key, metric)

	keys := sortedCases(baseVals, expVals)
	if len(keys) == 0 {
		return DeltaStats{}, false
	}
	diffs := make([]float64, len(keys))
	for i, k := range keys {
		diffs[i] = analysis.Mean(expVals[k]) - analysis.Mean(baseVals[k])
	}

	delta := analysis.Mean(diffs)
	low, high := analysis.Bootstrap(diffs, resamples, confidence, defaultSeed)
	return DeltaStats{Delta: delta, CILow: low, CIHigh: high, N: len(diffs)}, true
}

// verdicts indexes the gate verdicts of every metric by case, one verdict
// per sample.
func verdicts(results []ObservationResult, key keyer) map[string]map[pairKey][]bool {
	out := map[string]map[pairKey][]bool{}
	for _, r := range results {
		for _, s := range r.Scores {
			if s.Passed == nil {
				continue
			}
			m, ok := out[s.Name]
			if !ok {
				m = map[pairKey][]bool{}
				out[s.Name] = m
			}
			k := caseOf(key(r.Observation))
			m[k] = append(m[k], *s.Passed)
		}
	}
	return out
}

// pairedPasses builds [PassStats] for every metric gated on both sides.
func pairedPasses(base, exp []ObservationResult, key keyer, resamples int, confidence float64) map[string]PassStats {
	baseV, expV := verdicts(base, key), verdicts(exp, key)
	out := map[string]PassStats{}
	for metric, ev := range expV {
		bv, ok := baseV[metric]
		if !ok {
			continue
		}
		cases := sortedCases(bv, ev)
		if len(cases) == 0 {
			continue
		}
		single := true
		for _, k := range cases {
			if len(bv[k]) != 1 || len(ev[k]) != 1 {
				single = false
				break
			}
		}
		if single {
			out[metric] = mcNemarPasses(cases, bv, ev, confidence)
		} else {
			out[metric] = caseRatePasses(cases, bv, ev, resamples, confidence)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mcNemarPasses compares one verdict per case on each arm with McNemar's
// test.
func mcNemarPasses(cases []pairKey, bv, ev map[pairKey][]bool, confidence float64) PassStats {
	var p analysis.Paired
	for _, k := range cases {
		b, e := bv[k][0], ev[k][0]
		switch {
		case b && e:
			p.Both++
		case b:
			p.OnlyA++
		case e:
			p.OnlyB++
		default:
			p.Neither++
		}
	}
	n := p.N()
	return PassStats{
		Pairs:    p,
		Base:     analysis.Rate(p.Both+p.OnlyA, n, n, confidence),
		Exp:      analysis.Rate(p.Both+p.OnlyB, n, n, confidence),
		Diff:     p.Diff(confidence),
		McNemarP: p.McNemarP(),
	}
}

// caseRatePasses compares per-case pass rates when some case was sampled
// more than once. Each arm's rate and the paired difference are means over
// cases with a case-level bootstrap interval, so repeats never count as
// independent cases.
func caseRatePasses(cases []pairKey, bv, ev map[pairKey][]bool, resamples int, confidence float64) PassStats {
	rate := func(vs []bool) float64 {
		pass := 0
		for _, v := range vs {
			if v {
				pass++
			}
		}
		return float64(pass) / float64(len(vs))
	}
	baseRates := make([]float64, len(cases))
	expRates := make([]float64, len(cases))
	diffs := make([]float64, len(cases))
	for i, k := range cases {
		baseRates[i], expRates[i] = rate(bv[k]), rate(ev[k])
		diffs[i] = expRates[i] - baseRates[i]
	}
	stat := func(values []float64) Stat {
		low, high := analysis.Bootstrap(values, resamples, confidence, defaultSeed)
		return Stat{N: len(values), Value: analysis.Mean(values), Low: low, High: high, Completeness: 1}
	}
	return PassStats{
		Base:      stat(baseRates),
		Exp:       stat(expRates),
		Diff:      stat(diffs),
		McNemarP:  1,
		CaseRates: true,
	}
}

// diffKey identifies one case for the per-case diff, rolling up samples.
type diffKey struct {
	metric  string
	id      string
	turn    int
	variant string
}

// caseRollup accumulates one arm's samples of one case.
type caseRollup struct {
	values       []float64
	gated, pass  int
	inconclusive bool
}

func rollup(results []ObservationResult, key keyer) map[diffKey]*caseRollup {
	out := map[diffKey]*caseRollup{}
	for _, r := range results {
		for _, s := range r.Scores {
			if s.Name == "" {
				continue
			}
			pk := key(r.Observation)
			k := diffKey{s.Name, pk.id, pk.turn, pk.variant}
			c, ok := out[k]
			if !ok {
				c = &caseRollup{}
				out[k] = c
			}
			if s.Error == "" {
				c.values = append(c.values, s.Value)
			}
			c.inconclusive = c.inconclusive || s.Inconclusive
			if s.Passed != nil {
				c.gated++
				if *s.Passed {
					c.pass++
				}
			}
		}
	}
	return out
}

func (c *caseRollup) mean() *float64 {
	if c == nil || len(c.values) == 0 {
		return nil
	}
	m := analysis.Mean(c.values)
	return &m
}

func (c *caseRollup) passRate() *float64 {
	if c == nil || c.gated == 0 {
		return nil
	}
	r := float64(c.pass) / float64(c.gated)
	return &r
}

// unmeasured indexes the cases whose subject failed on infrastructure, which
// have no scores to roll up.
func unmeasured(results []ObservationResult, key keyer) map[pairKey]bool {
	out := map[pairKey]bool{}
	for _, r := range results {
		if SubjectInconclusive(r.Observation) {
			k := caseOf(key(r.Observation))
			out[k] = true
		}
	}
	return out
}

// caseDiffs builds the per-case diff between two arms.
func caseDiffs(base, exp []ObservationResult, key keyer, lowerIsBetter map[string]bool) []CaseDiff {
	baseR, expR := rollup(base, key), rollup(exp, key)
	baseU, expU := unmeasured(base, key), unmeasured(exp, key)
	inconclusive := func(r *caseRollup, u map[pairKey]bool, k diffKey) bool {
		return (r != nil && r.inconclusive) || u[pairKey{id: k.id, turn: k.turn, variant: k.variant}]
	}
	keys := make([]diffKey, 0, len(baseR)+len(expR))
	for k := range baseR {
		keys = append(keys, k)
	}
	for k := range expR {
		if _, ok := baseR[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(a, b diffKey) int {
		switch {
		case a.metric != b.metric:
			return cmp.Compare(a.metric, b.metric)
		case a.id != b.id:
			return cmp.Compare(a.id, b.id)
		case a.turn != b.turn:
			return a.turn - b.turn
		default:
			return cmp.Compare(a.variant, b.variant)
		}
	})

	out := make([]CaseDiff, 0, len(keys))
	for _, k := range keys {
		b, e := baseR[k], expR[k]
		d := CaseDiff{
			ID: k.id, Turn: k.turn, Variant: k.variant, Metric: k.metric,
			Base: b.mean(), Exp: e.mean(),
			BasePassRate: b.passRate(), ExpPassRate: e.passRate(),
		}
		unknown := inconclusive(b, baseU, k) || inconclusive(e, expU, k)
		if b.mean() == nil && e.mean() == nil && b.passRate() == nil && e.passRate() == nil {
			if unknown {
				d.Status = CaseInconclusive
				out = append(out, d)
			}
			// Otherwise every score errored on both sides: nothing to
			// compare.
			continue
		}
		d.Status = caseStatus(&d, lowerIsBetter[k.metric], unknown)
		out = append(out, d)
	}
	return out
}

// caseStatus sets d.Change and returns the status of a case with a value or
// verdict on at least one side. unknown reports that one arm could not
// measure the case.
func caseStatus(d *CaseDiff, lowerIsBetter, unknown bool) CaseStatus {
	var change float64
	var known bool
	switch {
	case d.BasePassRate != nil && d.ExpPassRate != nil:
		change, known = *d.ExpPassRate-*d.BasePassRate, true
		if d.Base != nil && d.Exp != nil {
			d.Change = *d.Exp - *d.Base
		}
	case d.Base != nil && d.Exp != nil:
		change, known = *d.Exp-*d.Base, true
		d.Change = change
		if lowerIsBetter {
			change = -change
		}
	}
	switch {
	case !known && unknown:
		return CaseInconclusive
	case !known && (d.Base != nil || d.BasePassRate != nil):
		return CaseOnlyBase
	case !known:
		return CaseOnlyExp
	case change > eqTolerance:
		return CaseImproved
	case change < -eqTolerance:
		return CaseRegressed
	default:
		return CaseUnchanged
	}
}

// copyObservations deep-copies the dataset fields of each observation:
// ID, Turn, Sample, Labels, Input, GroundTruth, and pre-seeded Annotations.
// Outputs and timing are left for the subject to fill.
func copyObservations(obs []Observation) []Observation {
	out := make([]Observation, len(obs))
	for i, o := range obs {
		out[i] = Observation{
			ID:          o.ID,
			Turn:        o.Turn,
			Sample:      o.Sample,
			Labels:      o.Labels.Clone(),
			Input:       append(json.RawMessage(nil), o.Input...),
			GroundTruth: append(json.RawMessage(nil), o.GroundTruth...),
		}
		if o.Annotations != nil {
			out[i].Annotations = make(map[string]json.RawMessage, len(o.Annotations))
			for k, v := range o.Annotations {
				out[i].Annotations[k] = append(json.RawMessage(nil), v...)
			}
		}
	}
	return out
}
