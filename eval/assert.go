package eval

import (
	"fmt"
	"math"
)

// Op is the comparison an [Assertion] applies: value Op threshold.
type Op string

// Comparison operators for [Assertion] and [PassRate].
const (
	GTE Op = ">="
	LTE Op = "<="
	EQ  Op = "=="
)

// eqTolerance absorbs floating-point noise in [EQ] comparisons.
const eqTolerance = 1e-9

// Holds reports whether value Op threshold is true. An unknown Op never holds.
func (op Op) Holds(value, threshold float64) bool {
	switch op {
	case GTE:
		return value >= threshold
	case LTE:
		return value <= threshold
	case EQ:
		return math.Abs(value-threshold) <= eqTolerance
	default:
		return false
	}
}

func (op Op) valid() bool { return op == GTE || op == LTE || op == EQ }

// Scope selects what an [Assertion] is checked against.
type Scope string

const (
	// EveryCase checks each observation's score. It is the default.
	EveryCase Scope = "every_case"
	// OnAggregate checks the suite's mean for the metric.
	OnAggregate Scope = "aggregate"
)

// Assertion is a pass/fail gate on one metric, such as "faithfulness >= 0.8
// on every case" or "tool_success_rate == 1 on aggregate".
type Assertion struct {
	Metric    string  `json:"metric"`
	Op        Op      `json:"op"`
	Threshold float64 `json:"threshold"`
	// Scope defaults to [EveryCase] when empty.
	Scope Scope `json:"scope,omitempty"`
	// Where limits the assertion to observations whose labels match, such
	// as one variant of an experiment. Empty means every observation.
	Where Where `json:"where,omitempty"`
	// MinPassRate relaxes an [EveryCase] assertion: it holds when at least
	// this fraction of the in-scope scores hold, instead of all of them.
	// Zero means every score must hold. Errored scores count as failures;
	// inconclusive scores (see [Score.Inconclusive]) are left out.
	MinPassRate float64 `json:"min_pass_rate,omitempty"`
}

// Outcome is the answer of a gated suite.
type Outcome string

const (
	// OutcomePassed means every assertion held.
	OutcomePassed Outcome = "passed"
	// OutcomeFailed means at least one assertion was violated by a real
	// result: the subject answered and was judged wrong, or it failed for
	// a reason that is not infrastructure.
	OutcomeFailed Outcome = "failed"
	// OutcomeInconclusive means no assertion was violated by a real
	// result, but more results than the [GatePolicy] tolerates could not
	// be measured because infrastructure failed (see [IsInfra]). Rerun the
	// suite; the gate has not decided.
	OutcomeInconclusive Outcome = "inconclusive"
	// OutcomeUnasserted means the suite was gated with no assertions, so it
	// is a measurement only.
	OutcomeUnasserted Outcome = "unasserted"
)

// ViolationKind says what a [Violation] is about, so callers and reports can
// tell real failures from inconclusive ones without parsing Reason.
type ViolationKind string

// Violation kinds. Violations stored before kinds existed have an empty kind
// and count as real failures.
const (
	// ViolationMetric is a value that broke its threshold, a metric that
	// never scored, or an invalid assertion.
	ViolationMetric ViolationKind = "metric"
	// ViolationScorer is a scorer that failed for a reason other than
	// infrastructure.
	ViolationScorer ViolationKind = "scorer"
	// ViolationSubject is a subject that failed for a reason other than
	// infrastructure.
	ViolationSubject ViolationKind = "subject"
	// ViolationInconclusive is a result that could not be measured: a
	// subject or scorer that failed on infrastructure, an incomplete
	// suite, or a metric with no conclusive result to check.
	ViolationInconclusive ViolationKind = "inconclusive"
)

// Violation is one failed [Assertion]. CaseID is empty for aggregate and
// suite-level violations. Reason explains violations that have no value to
// compare, such as an errored score or a metric that never appeared.
type Violation struct {
	Kind      ViolationKind `json:"kind,omitempty"`
	CaseID    string        `json:"case_id,omitempty"`
	Turn      int           `json:"turn,omitempty"`
	Sample    int           `json:"sample,omitempty"`
	Metric    string        `json:"metric"`
	Op        Op            `json:"op,omitempty"`
	Value     float64       `json:"value"`
	Threshold float64       `json:"threshold"`
	Reason    string        `json:"reason,omitempty"`
	// Where is the scope of the violated assertion, if it had one.
	Where Where `json:"where,omitempty"`
}

// Inconclusive reports whether the violation is about a result that could
// not be measured rather than a real failure.
func (v Violation) Inconclusive() bool { return v.Kind == ViolationInconclusive }

func (v Violation) String() string {
	where := "aggregate"
	if v.CaseID != "" {
		where = "case " + v.CaseID
	}
	if len(v.Where) > 0 {
		where += " " + v.Where.String()
	}
	if v.Inconclusive() {
		where = "inconclusive: " + where
	}
	if v.Reason != "" {
		if v.Metric == "" {
			return fmt.Sprintf("%s: %s", where, v.Reason)
		}
		return fmt.Sprintf("%s %s: %s", where, v.Metric, v.Reason)
	}
	return fmt.Sprintf("%s %s: %g is not %s %g", where, v.Metric, v.Value, v.Op, v.Threshold)
}

// Check evaluates assertions against the suite and returns every violation;
// an empty result means all gates passed, so CI can gate on
// len(suite.Check(...)) == 0. It reads only the stored results, so it works
// on a suite loaded with [ReadSuiteResult].
//
// For [EveryCase], an errored score is a violation and an observation the
// scorer declined is skipped; with MinPassRate set, case violations are
// reported only when the pass rate falls below it, followed by one summary
// violation carrying the rate. A metric that never scored anywhere is a
// violation, so a misspelled metric name cannot pass silently. An incomplete
// suite, or one with subject failures, yields one suite-level violation each,
// since its scores do not cover the dataset. To require a gate to hold on
// every sample of a sampled scorer, sample with the [Min] reducer.
//
// Results that could not be measured yield [ViolationInconclusive]
// violations instead: an inconclusive score (see [Score.Inconclusive]), the
// subject failures that [SubjectInconclusive] reports, an incomplete suite,
// and a metric missing from a scope where no result was conclusive. They are
// left out of MinPassRate. Every violation still counts here, so
// len(Check(...)) == 0 means every gate was checked and held.
func (s *SuiteResult) Check(assertions ...Assertion) []Violation {
	var out []Violation
	if s.Incomplete > 0 {
		out = append(out, Violation{Kind: ViolationInconclusive, Reason: fmt.Sprintf("%d observations incomplete", s.Incomplete)})
	}
	infra := 0
	for _, r := range s.Results {
		if SubjectInconclusive(r.Observation) {
			infra++
		}
	}
	// SubjectErrors can be set without results, as on a summary; whatever
	// the results do not mark as infrastructure is a real failure.
	if real := s.SubjectErrors - infra; real > 0 {
		out = append(out, Violation{Kind: ViolationSubject, Reason: fmt.Sprintf("subject failed on %d observations", real)})
	}
	if infra > 0 {
		out = append(out, Violation{Kind: ViolationInconclusive, Reason: fmt.Sprintf("subject failed on infrastructure on %d observations", infra)})
	}
	for _, a := range assertions {
		out = append(out, s.check(a)...)
	}
	return out
}

// noConclusiveResult reports whether nothing in scope could have scored a
// metric: every matching result is inconclusive, and at least one result is
// inconclusive or the suite is incomplete. A metric missing from such a
// scope is inconclusive rather than misspelled.
func (s *SuiteResult) noConclusiveResult(where Where) bool {
	unmeasured := s.Incomplete > 0
	for _, r := range s.Results {
		if !where.Match(r.Observation) {
			continue
		}
		if !r.Inconclusive() {
			return false
		}
		unmeasured = true
	}
	return unmeasured
}

func (s *SuiteResult) check(a Assertion) []Violation {
	base := Violation{Kind: ViolationMetric, Metric: a.Metric, Op: a.Op, Threshold: a.Threshold, Where: a.Where}
	if !a.Op.valid() {
		base.Reason = fmt.Sprintf("unknown op %q", a.Op)
		return []Violation{base}
	}

	if a.Scope == OnAggregate {
		agg := s.Aggregate
		if len(a.Where) > 0 {
			agg = Aggregate(a.Where.Filter(s.Results))
		}
		value, ok := agg[a.Metric]
		if !ok {
			base.Reason = "metric has no successful scores"
			if s.noConclusiveResult(a.Where) {
				base.Kind = ViolationInconclusive
			}
			return []Violation{base}
		}
		if !a.Op.Holds(value, a.Threshold) {
			base.Value = value
			return []Violation{base}
		}
		return nil
	}
	if a.Scope != "" && a.Scope != EveryCase {
		base.Reason = fmt.Sprintf("unknown scope %q", a.Scope)
		return []Violation{base}
	}

	var out []Violation
	seen, held, inconclusive := 0, 0, 0
	for _, r := range s.Results {
		if !a.Where.Match(r.Observation) {
			continue
		}
		for _, sc := range r.Scores {
			if sc.Name != a.Metric {
				continue
			}
			seen++
			v := base
			v.CaseID, v.Turn, v.Sample = r.Observation.ID, r.Observation.Turn, r.Observation.Sample
			switch {
			case sc.Error != "" && sc.Inconclusive:
				inconclusive++
				v.Kind = ViolationInconclusive
				v.Reason = "scorer failed on infrastructure: " + sc.Error
			case sc.Error != "":
				v.Kind = ViolationScorer
				v.Reason = "scorer error: " + sc.Error
			case !a.Op.Holds(sc.Value, a.Threshold):
				v.Value = sc.Value
			default:
				held++
				continue
			}
			out = append(out, v)
		}
	}
	if seen == 0 {
		base.Reason = "metric not found in any result"
		if len(a.Where) > 0 {
			base.Reason = "metric not found in any result matching " + a.Where.String()
		}
		if s.noConclusiveResult(a.Where) {
			base.Kind = ViolationInconclusive
		}
		return []Violation{base}
	}
	if a.MinPassRate > 0 {
		// Inconclusive scores stay listed but are not graded.
		var kept []Violation
		for _, v := range out {
			if v.Inconclusive() {
				kept = append(kept, v)
			}
		}
		graded := seen - inconclusive
		if graded == 0 {
			return kept
		}
		rate := float64(held) / float64(graded)
		if rate >= a.MinPassRate {
			return kept
		}
		base.Value = rate
		base.Reason = fmt.Sprintf("pass rate %.3g over %d scores is below %.3g", rate, graded, a.MinPassRate)
		out = append(out, base)
	}
	return out
}

// Gate checks assertions like [SuiteResult.Check] and records the result on
// the suite: Violations, Outcome, and [Score.Passed] on every score an
// [EveryCase] assertion covers. Gating again replaces the previous result.
// With no assertions the outcome is [OutcomeUnasserted]. It is
// [SuiteResult.GateWith] with the zero [GatePolicy], which tolerates no
// inconclusive case.
func (s *SuiteResult) Gate(assertions ...Assertion) Outcome {
	return s.GateWith(GatePolicy{}, assertions...)
}

// GateWith gates the suite like [SuiteResult.Gate] under policy. The outcome
// is [OutcomeFailed] when any violation is a real failure, whatever the
// policy. Otherwise it is [OutcomeInconclusive] when some violation is
// inconclusive and the suite's [SuiteResult.InconclusiveRate] exceeds
// policy.MaxInconclusive, and [OutcomePassed] when it does not.
// Inconclusive scores get no Passed verdict, so pass rates leave them out.
func (s *SuiteResult) GateWith(policy GatePolicy, assertions ...Assertion) Outcome {
	for i := range s.Results {
		for j := range s.Results[i].Scores {
			s.Results[i].Scores[j].Passed = nil
		}
	}
	if len(assertions) == 0 {
		s.Violations = nil
		s.Outcome = OutcomeUnasserted
		return s.Outcome
	}
	for _, a := range assertions {
		if !a.Op.valid() || (a.Scope != "" && a.Scope != EveryCase) {
			continue
		}
		for i := range s.Results {
			r := &s.Results[i]
			if !a.Where.Match(r.Observation) {
				continue
			}
			for j := range r.Scores {
				sc := &r.Scores[j]
				if sc.Name != a.Metric {
					continue
				}
				if sc.Inconclusive {
					continue
				}
				ok := sc.Error == "" && a.Op.Holds(sc.Value, a.Threshold)
				if sc.Passed != nil {
					ok = ok && *sc.Passed
				}
				sc.Passed = &ok
			}
		}
	}
	s.Violations = s.Check(assertions...)
	s.Outcome = OutcomePassed
	inconclusive := false
	for _, v := range s.Violations {
		if !v.Inconclusive() {
			s.Outcome = OutcomeFailed
			return s.Outcome
		}
		inconclusive = true
	}
	if inconclusive && s.InconclusiveRate() > policy.MaxInconclusive {
		s.Outcome = OutcomeInconclusive
	}
	return s.Outcome
}

// PassRate returns the fraction of scored observations whose metric value
// satisfies value Op threshold. Errored scores count as failures; inconclusive
// scores (see [Score.Inconclusive]) and observations the scorer declined are
// not counted. It returns 0 when the metric has no graded scores.
func PassRate(results []ObservationResult, metric string, op Op, threshold float64) float64 {
	var pass, total int
	for _, r := range results {
		for _, s := range r.Scores {
			if s.Name != metric || s.Inconclusive {
				continue
			}
			total++
			if s.Error == "" && op.Holds(s.Value, threshold) {
				pass++
			}
		}
	}
	if total == 0 {
		return 0
	}
	return float64(pass) / float64(total)
}
