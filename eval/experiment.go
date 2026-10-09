package eval

import (
	"context"
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
	Name string
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
	if err := e.validate(); err != nil {
		return nil, err
	}
	if e.Sampler.N > 1 {
		opts = append([]Option{WithSampler(e.Sampler)}, opts...)
	}
	cfg := newConfig(opts)
	dataset = Sampler{N: cfg.Repeats}.Replicate(dataset)

	all := make([]Observation, 0, len(dataset)*len(e.Variants))
	for _, v := range e.Variants {
		obs := copyObservations(dataset)
		labels := v.Labels.merge(Labels{LabelVariant: v.Name})
		for i := range obs {
			obs[i].Labels = obs[i].Labels.merge(labels)
		}
		_ = PopulateAll(ctx, obs, v.Subject, opts...)
		all = append(all, obs...)
	}

	runOpts := append(append([]Option(nil), opts...), WithAssertions(e.Assertions...))
	suite, err := Run(ctx, e.Name, all, e.Scorers, runOpts...)
	if suite != nil {
		suite.Claim = e.Claim
		if len(e.Assertions) == 0 && len(cfg.Assertions) == 0 {
			suite.Gate()
		}
	}
	return suite, err
}

func (e Experiment) validate() error {
	if len(e.Variants) == 0 {
		return fmt.Errorf("experiment %q: no variants", e.Name)
	}
	seen := make(map[string]bool, len(e.Variants))
	for i, v := range e.Variants {
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
		if v.Subject == nil {
			return fmt.Errorf("experiment %q: variant %q has no subject", e.Name, v.Name)
		}
	}
	return nil
}
