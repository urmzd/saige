package eval

import (
	"context"
	"encoding/json"
	"fmt"
)

// Experiment states what a run should learn: a claim, the variants under
// test, how to score them, and which gates must hold. It owns no data; a
// run pairs it with a dataset, so the same experiment can be rerun on a new
// or larger dataset.
//
// To gate variants separately, give each assertion a [Where] on
// [LabelVariant]. To compare variants against a control, pass the result
// to [CompareVariants].
type Experiment struct {
	// Revision pins implementation or external settings; Config records the knobs.
	Revision string
	Config   json.RawMessage
	Name     string
	// Claim is the hypothesis in plain words, such as "retrieval improves
	// answer correctness on billing questions". It is copied to
	// [SuiteResult.Claim].
	Claim      string
	Variants   []Variant
	Scorers    []Scorer
	Assertions []Assertion
	// Sampler samples the scorers as [WithSampler] does. A [WithSampler]
	// option passed to Run takes precedence.
	Sampler Sampler
}

// Run runs every variant's subject over the dataset, scores all results in
// one suite, and gates it with the experiment's assertions.
//
// Each observation is copied once per variant and labeled with the variant's
// labels and its name under [LabelVariant]; variant labels override dataset
// labels with the same key. Options apply as in [Run] and [PopulateAll], and
// [WithRepeats] runs each subject several times per observation. A subject
// error on one observation is recorded on it and the rest continue. The
// returned error is the error of [Run], so a cancelled experiment returns
// the partial suite with an error wrapping ctx.Err().
func (e Experiment) Run(ctx context.Context, dataset []Observation, opts ...Option) (*SuiteResult, error) {
	result, err := e.Sample(ctx, dataset, 1, opts...)
	if result == nil || len(result.Evaluations) == 0 {
		return nil, err
	}
	return result.Evaluations[0], err
}

func (e Experiment) validateSetup(requireSubjects bool) error {
	if len(e.Config) > 0 && !json.Valid(e.Config) {
		return fmt.Errorf("experiment %q: invalid config JSON", e.Name)
	}
	if len(e.Variants) == 0 {
		return fmt.Errorf("experiment %q: no variants", e.Name)
	}
	seen := make(map[string]bool, len(e.Variants))
	for i, v := range e.Variants {
		if len(v.Config) > 0 && !json.Valid(v.Config) {
			return fmt.Errorf("experiment %q: variant %q has invalid config JSON", e.Name, v.Name)
		}
		if v.Name == "" {
			return fmt.Errorf("experiment %q: variant %d has no name", e.Name, i)
		}
		if lv, ok := v.Labels[LabelVariant]; ok && lv != v.Name {
			return fmt.Errorf("experiment %q: variant %q has label %s=%q; it must match the name", e.Name, v.Name, LabelVariant, lv)
		}
		if seen[v.Name] {
			return fmt.Errorf("experiment %q: duplicate variant %q", e.Name, v.Name)
		}
		seen[v.Name] = true
		if requireSubjects && v.Subject == nil {
			return fmt.Errorf("experiment %q: variant %q has no subject", e.Name, v.Name)
		}
	}
	return nil
}
