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
	// Zero means every score must hold. Errored scores count as failures.
	MinPassRate float64 `json:"min_pass_rate,omitempty"`
}

// Outcome is the three-valued answer of a gated suite.
type Outcome string

const (
	// OutcomePassed means every assertion held.
	OutcomePassed Outcome = "passed"
	// OutcomeFailed means at least one assertion was violated.
	OutcomeFailed Outcome = "failed"
	// OutcomeUnasserted means the suite was gated with no assertions, so it
	// is a measurement only.
	OutcomeUnasserted Outcome = "unasserted"
)

// Violation is one failed [Assertion]. CaseID is empty for aggregate and
// suite-level violations. Reason explains violations that have no value to
// compare, such as an errored score or a metric that never appeared.
type Violation struct {
	CaseID    string  `json:"case_id,omitempty"`
	Turn      int     `json:"turn,omitempty"`
	Sample    int     `json:"sample,omitempty"`
	Metric    string  `json:"metric"`
	Op        Op      `json:"op,omitempty"`
	Value     float64 `json:"value"`
	Threshold float64 `json:"threshold"`
	Reason    string  `json:"reason,omitempty"`
	// Where is the scope of the violated assertion, if it had one.
	Where Where `json:"where,omitempty"`
}

func (v Violation) String() string {
	where := "aggregate"
	if v.CaseID != "" {
		where = "case " + v.CaseID
	}
	if len(v.Where) > 0 {
		where += " " + v.Where.String()
	}
	if v.Reason != "" {
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
func (s *SuiteResult) Check(assertions ...Assertion) []Violation {
	var out []Violation
	if s.Incomplete > 0 {
		out = append(out, Violation{Reason: fmt.Sprintf("%d observations incomplete", s.Incomplete)})
	}
	if s.SubjectErrors > 0 {
		out = append(out, Violation{Reason: fmt.Sprintf("subject failed on %d observations", s.SubjectErrors)})
	}
	for _, a := range assertions {
		out = append(out, s.check(a)...)
	}
	return out
}

func (s *SuiteResult) check(a Assertion) []Violation {
	base := Violation{Metric: a.Metric, Op: a.Op, Threshold: a.Threshold, Where: a.Where}
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
	seen, held := 0, 0
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
			case sc.Error != "":
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
		return []Violation{base}
	}
	if a.MinPassRate > 0 {
		rate := float64(held) / float64(seen)
		if rate >= a.MinPassRate {
			return nil
		}
		base.Value = rate
		base.Reason = fmt.Sprintf("pass rate %.3g over %d scores is below %.3g", rate, seen, a.MinPassRate)
		out = append(out, base)
	}
	return out
}

// Gate checks assertions like [SuiteResult.Check] and records the result on
// the suite: Violations, Outcome, and [Score.Passed] on every score an
// [EveryCase] assertion covers. Gating again replaces the previous result.
// With no assertions the outcome is [OutcomeUnasserted].
func (s *SuiteResult) Gate(assertions ...Assertion) Outcome {
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
	if len(s.Violations) > 0 {
		s.Outcome = OutcomeFailed
	}
	return s.Outcome
}

// PassRate returns the fraction of scored observations whose metric value
// satisfies value Op threshold. Errored scores count as failures; observations
// the scorer declined are not counted. It returns 0 when the metric has no
// scores at all.
func PassRate(results []ObservationResult, metric string, op Op, threshold float64) float64 {
	var pass, total int
	for _, r := range results {
		for _, s := range r.Scores {
			if s.Name != metric {
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
