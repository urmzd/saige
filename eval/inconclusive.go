package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/urmzd/saige/agent/types"
)

// An evaluation can fail to measure. When the subject or a scorer could not
// produce a result because the infrastructure failed (a provider outage, a
// rate limit, a timeout, a cancelled run, an unreachable connector, bad
// credentials), the result says nothing about the system under test. Such
// results are inconclusive: they are kept out of pass rates and gate
// decisions and counted separately, so an outage does not read as a
// regression and a broken run does not read as a pass.
//
// The rule is: a result is a real failure only when the subject answered and
// the answer was judged wrong, or when the subject or scorer failed for a
// reason a retry cannot fix (an invalid request, a context overflow, a
// content filter, a truncated reply, an unclassified error). Everything
// [IsInfra] reports is inconclusive.

// ErrInfra marks an infrastructure failure. Wrap an error with [Infra], or
// with fmt.Errorf("...: %w", eval.ErrInfra), to have [IsInfra] report it, for
// example when a subject finds a connector it needs unreachable.
var ErrInfra = errors.New("infrastructure failure")

// AnnotationSubjectInconclusive is the annotation key [PopulateAll] sets, to
// JSON true, when the subject error it records under
// [AnnotationSubjectError] is an infrastructure failure (see [IsInfra]).
const AnnotationSubjectInconclusive = "eval.subject_inconclusive"

type infraError struct{ err error }

func (e infraError) Error() string { return e.err.Error() }
func (e infraError) Unwrap() error { return e.err }

func (infraError) Is(target error) bool { return target == ErrInfra }

// Infra marks err as an infrastructure failure, so [IsInfra] reports it and
// a subject or scorer that returns it yields an inconclusive result. It
// returns nil for nil.
func Infra(err error) error {
	if err == nil {
		return nil
	}
	return infraError{err}
}

// IsInfra reports whether err is an infrastructure failure rather than a
// failure of the system under test: an error marked with [ErrInfra],
// a context cancellation or deadline, a provider error whose kind is
// transient (rate limit, unavailable, other retryable statuses) or an
// authentication failure, or a network failure that
// [types.ClassifyTransportError] recognizes, such as a refused or reset
// connection. Every other error, nil included, is not.
func IsInfra(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrInfra) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if kind := types.KindOf(err); kind.Transient() || kind == types.ErrorKindAuth {
		return true
	}
	_, transport := types.ClassifyTransportError(err)
	return transport
}

// SubjectInconclusive reports whether the subject error recorded on obs is an
// infrastructure failure. Observations recorded before this distinction
// existed carry no marker and report false, so their subject errors keep
// counting as failures.
func SubjectInconclusive(obs Observation) bool {
	raw, ok := obs.Annotations[AnnotationSubjectInconclusive]
	if !ok || SubjectError(obs) == "" {
		return false
	}
	var v bool
	return json.Unmarshal(raw, &v) == nil && v
}

// MarkSubjectError records err on obs as [PopulateAll] does: the message
// under [AnnotationSubjectError] and, when [IsInfra] reports it, the
// [AnnotationSubjectInconclusive] marker. Code that builds observations for
// a failed subject itself, such as a harness, uses it to keep the
// classification. A nil err clears both annotations.
func MarkSubjectError(obs *Observation, err error) {
	if err == nil {
		delete(obs.Annotations, AnnotationSubjectError)
		delete(obs.Annotations, AnnotationSubjectInconclusive)
		return
	}
	msg, _ := json.Marshal(err.Error())
	if obs.Annotations == nil {
		obs.Annotations = map[string]json.RawMessage{}
	}
	obs.Annotations[AnnotationSubjectError] = msg
	if IsInfra(err) {
		obs.Annotations[AnnotationSubjectInconclusive] = json.RawMessage("true")
	} else {
		delete(obs.Annotations, AnnotationSubjectInconclusive)
	}
}

// Inconclusive reports whether the result could not be measured: its subject
// failed on infrastructure, or one of its scores is inconclusive (see
// [Score.Inconclusive]).
func (r ObservationResult) Inconclusive() bool {
	if SubjectInconclusive(r.Observation) {
		return true
	}
	for _, s := range r.Scores {
		if s.Inconclusive {
			return true
		}
	}
	return false
}

// countInconclusive counts the inconclusive results.
func countInconclusive(results []ObservationResult) int {
	n := 0
	for _, r := range results {
		if r.Inconclusive() {
			n++
		}
	}
	return n
}

// InconclusiveRate returns the share of the suite's cases that are
// inconclusive. A case is one observation ID; it is inconclusive when any
// of its results is (see [ObservationResult.Inconclusive]). Each incomplete
// observation counts as one more inconclusive case, since its ID is not
// recorded. It returns 0 for an empty suite.
func (s *SuiteResult) InconclusiveRate() float64 {
	cases := map[string]bool{}
	for _, r := range s.Results {
		id := r.Observation.ID
		cases[id] = cases[id] || r.Inconclusive()
	}
	total := len(cases) + s.Incomplete
	if total == 0 {
		return 0
	}
	bad := s.Incomplete
	for _, inconclusive := range cases {
		if inconclusive {
			bad++
		}
	}
	return float64(bad) / float64(total)
}

// GatePolicy tunes how [SuiteResult.GateWith] treats inconclusive results.
type GatePolicy struct {
	// MaxInconclusive is the largest share of cases (see
	// [SuiteResult.InconclusiveRate]) that may be inconclusive while the
	// gate still decides on the rest. Above it, a gate with no real
	// violation is [OutcomeInconclusive]. Zero, the default, tolerates
	// none: any inconclusive case makes such a gate inconclusive.
	MaxInconclusive float64
}

// Validate rejects a MaxInconclusive outside [0, 1].
func (p GatePolicy) Validate() error {
	if p.MaxInconclusive < 0 || p.MaxInconclusive > 1 {
		return fmt.Errorf("max inconclusive %g must be between 0 and 1", p.MaxInconclusive)
	}
	return nil
}
