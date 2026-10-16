package eval

import (
	"fmt"
	"math"

	"github.com/urmzd/saige/eval/analysis"
)

// Reliability describes repeated grading of the same fixed trace. Agreement
// compares each thresholded verdict with the caller's desired verdict, not
// with other judge calls. Error and declined attempts reduce completeness.
type Reliability struct {
	Revision      string  `json:"revision"`
	Confidence    float64 `json:"confidence"`
	Passed        int     `json:"passed"`
	PassRate      Stat    `json:"pass_rate"`
	AgreementRate Stat    `json:"agreement_rate"`
	// PassAtK is the unbiased probability of at least one passing draw in k
	// draws without replacement. Undefined k values are omitted.
	PassAtK map[int]float64 `json:"pass_at_k,omitempty"`
}

// Reliability thresholds each successful sample at >= threshold and reports
// Wilson intervals for pass and agreement rates. desired is the expected
// pass/fail verdict for this trace. It is independent of the score reducer.
// Repeated judge calls must be fresh draws: disable response caching when
// estimating judge uncertainty. These intervals describe this trace only.
func (st SampleStats) Reliability(threshold float64, desired bool, ks ...int) (Reliability, error) {
	if math.IsNaN(threshold) || math.IsInf(threshold, 0) {
		return Reliability{}, fmt.Errorf("reliability: threshold must be finite")
	}
	for _, v := range st.Values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return Reliability{}, fmt.Errorf("reliability: samples must be finite")
		}
	}
	confidence := st.Confidence
	if confidence <= 0 || confidence >= 1 || math.IsNaN(confidence) {
		confidence = DefaultConfidence
	}
	n := len(st.Values)
	expected := st.Expected
	if expected == 0 {
		expected = n + st.Errors
	}
	passed := 0
	for _, v := range st.Values {
		if v >= threshold {
			passed++
		}
	}
	agreed := passed
	if !desired {
		agreed = n - passed
	}
	r := Reliability{
		Revision:   RevisionTag("reliability/v1", st.Revision, threshold, desired, ks, confidence),
		Confidence: confidence, Passed: passed,
		PassRate:      analysis.Rate(passed, n, expected, confidence),
		AgreementRate: analysis.Rate(agreed, n, expected, confidence),
	}
	for _, k := range ks {
		if v := analysis.PassAtK(n, passed, k); !math.IsNaN(v) {
			if r.PassAtK == nil {
				r.PassAtK = map[int]float64{}
			}
			r.PassAtK[k] = v
		}
	}
	return r, nil
}
