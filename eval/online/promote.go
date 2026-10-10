package online

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/urmzd/saige/agent/privacy"
	"github.com/urmzd/saige/eval"
)

// Annotations a promoted case carries.
const (
	// AnnotationRubric is the grading rubric of a promoted case, as a JSON
	// string, for a judge to read in place of a ground truth.
	AnnotationRubric = "rubric"
	// LabelPromotedFrom names the online run a case was promoted from.
	LabelPromotedFrom = "promoted_from"
)

// LabelFlagged marks a unit a reviewer or a scorer flagged for promotion
// regardless of its scores, when set to "true".
const LabelFlagged = "flagged"

// Case is one dataset case promoted from a production run.
type Case struct {
	ID    string          `json:"id"`
	Input json.RawMessage `json:"input"`
	// Expected is the expected output, when the promotion supplied one.
	Expected json.RawMessage `json:"expected,omitempty"`
	// Rubric says what a good answer must do, when there is no expected
	// output to compare with.
	Rubric string      `json:"rubric,omitempty"`
	Labels eval.Labels `json:"labels,omitempty"`
	// Reasons lists why the case was promoted: the failed scores and their
	// reasons, or the flag.
	Reasons []string `json:"reasons"`
}

// Observation returns the case as an eval observation with the expected
// output as ground truth and the rubric under AnnotationRubric.
func (c Case) Observation() eval.Observation {
	obs := eval.Observation{ID: c.ID, Labels: c.Labels.Clone(), Input: c.Input, GroundTruth: c.Expected}
	if c.Rubric != "" {
		raw, _ := json.Marshal(c.Rubric)
		obs.Annotations = map[string]json.RawMessage{AnnotationRubric: raw}
	}
	return obs
}

// PromoteOptions configures [Promote].
type PromoteOptions struct {
	// Failing decides whether a unit is promoted, returning the reasons
	// when it is. Nil promotes flagged units, failed gates, and errored
	// scores: [Failing] with no metrics.
	Failing func(eval.Unit) []string
	// Expected returns the expected output for a promoted unit, such as a
	// reviewer's corrected answer. Nil, or a nil result, leaves the case
	// without one.
	Expected func(eval.Unit) json.RawMessage
	// Rubric returns the rubric for a promoted unit. Nil derives one from
	// the reasons the unit failed.
	Rubric func(eval.Unit, []string) string
	// Detector finds personal data to redact; nil uses the privacy
	// package's default detector. Every string in the input, the expected
	// output, the rubric, and the reasons is redacted before the case is
	// returned.
	Detector privacy.Detector
}

// Failing returns a Failing function for [PromoteOptions] that promotes a
// unit when it is flagged ([LabelFlagged]), when a gate failed one of its
// scores, when a scorer errored, or when one of the listed metrics scored
// below threshold. Only listed metrics are compared with the threshold,
// since counts, latencies, and costs are not on a 0 to 1 scale. A score that
// is inconclusive (see [eval.Score.Inconclusive]), such as a judge whose
// provider was down, says nothing about the run and never promotes it.
func Failing(threshold float64, metrics ...string) func(eval.Unit) []string {
	return func(u eval.Unit) []string {
		var reasons []string
		if u.Observation.Labels[LabelFlagged] == "true" {
			reasons = append(reasons, "flagged")
		}
		for _, sc := range u.Scores {
			switch {
			case sc.Inconclusive:
			case sc.Error != "":
				reasons = append(reasons, fmt.Sprintf("%s errored: %s", sc.Name, sc.Error))
			case sc.Passed != nil && !*sc.Passed:
				reasons = append(reasons, scoreReason(sc, "failed its gate"))
			case slices.Contains(metrics, sc.Name) && sc.Value < threshold:
				reasons = append(reasons, scoreReason(sc, fmt.Sprintf("scored %.3g, below %.3g", sc.Value, threshold)))
			}
		}
		return reasons
	}
}

func scoreReason(sc eval.Score, what string) string {
	if sc.Reason == "" {
		return sc.Name + " " + what
	}
	return sc.Name + " " + what + ": " + sc.Reason
}

// Promote turns the units that fail into dataset cases, in order, with
// personal data redacted. A case keeps the unit's provenance labels
// (conversation, node, model, preset) and adds LabelPromotedFrom with the
// unit's run. Redaction is irreversible: a case never holds a value the
// detector found.
func Promote(ctx context.Context, units []eval.Unit, opts PromoteOptions) ([]Case, error) {
	failing := opts.Failing
	if failing == nil {
		failing = Failing(0)
	}
	var cases []Case
	for _, u := range units {
		reasons := failing(u)
		if len(reasons) == 0 {
			continue
		}
		c := Case{
			ID:      u.Observation.ID,
			Input:   u.Observation.Input,
			Labels:  promotedLabels(u),
			Reasons: reasons,
		}
		if opts.Expected != nil {
			c.Expected = opts.Expected(u)
		}
		if opts.Rubric != nil {
			c.Rubric = opts.Rubric(u, reasons)
		} else if c.Expected == nil {
			c.Rubric = defaultRubric(reasons)
		}
		redacted, err := redactCase(ctx, opts.Detector, c)
		if err != nil {
			return nil, fmt.Errorf("online: redact case %s: %w", c.ID, err)
		}
		cases = append(cases, redacted)
	}
	return cases, nil
}

func promotedLabels(u eval.Unit) eval.Labels {
	labels := eval.Labels{}
	for _, k := range []string{LabelConversation, LabelNode, LabelModel, LabelPreset, LabelScope} {
		if v, ok := u.Observation.Labels[k]; ok {
			labels[k] = v
		}
	}
	labels[LabelPromotedFrom] = u.RunID
	return labels
}

func defaultRubric(reasons []string) string {
	var b strings.Builder
	b.WriteString("A production answer to this input failed review. A correct answer must avoid these failures:")
	for _, r := range reasons {
		b.WriteString("\n- ")
		b.WriteString(r)
	}
	return b.String()
}

func redactCase(ctx context.Context, d privacy.Detector, c Case) (Case, error) {
	var err error
	if c.Input, err = redactJSON(ctx, d, c.Input); err != nil {
		return c, err
	}
	if c.Expected, err = redactJSON(ctx, d, c.Expected); err != nil {
		return c, err
	}
	if c.Rubric, err = privacy.Redact(ctx, d, c.Rubric); err != nil {
		return c, err
	}
	reasons := make([]string, len(c.Reasons))
	for i, r := range c.Reasons {
		if reasons[i], err = privacy.Redact(ctx, d, r); err != nil {
			return c, err
		}
	}
	c.Reasons = reasons
	return c, nil
}

// redactJSON redacts every string in a JSON document, keys included, and
// re-encodes it, so a redaction can never break the document's syntax.
func redactJSON(ctx context.Context, d privacy.Detector, raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return raw, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	v, err := redactValue(ctx, d, v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

func redactValue(ctx context.Context, d privacy.Detector, v any) (any, error) {
	switch x := v.(type) {
	case string:
		return privacy.Redact(ctx, d, x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			r, err := redactValue(ctx, d, e)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(x))
		for _, k := range slices.Sorted(maps.Keys(x)) {
			rk, err := privacy.Redact(ctx, d, k)
			if err != nil {
				return nil, err
			}
			r, err := redactValue(ctx, d, x[k])
			if err != nil {
				return nil, err
			}
			out[rk] = r
		}
		return out, nil
	default:
		return v, nil
	}
}

// WriteCases writes cases as JSON Lines, one case per line.
func WriteCases(w io.Writer, cases []Case) error {
	enc := json.NewEncoder(w)
	for _, c := range cases {
		if err := enc.Encode(c); err != nil {
			return err
		}
	}
	return nil
}
