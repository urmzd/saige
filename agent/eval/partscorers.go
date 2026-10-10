package eval

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/urmzd/saige/agent/types"
	topeval "github.com/urmzd/saige/eval"
)

// Part scorers read the final turn's parts recorded under
// [AnnotationParts]. Each scores 1 when its check holds and 0 when it does
// not, with a reason. They decline (return an empty Score) an observation
// with no parts recorded, such as one stored before parts existed, and are
// marked [topeval.Deterministic]: no LLM judge is involved.

// partsScorer builds a deterministic scorer over the recorded parts.
func partsScorer(metric string, check func(obs topeval.Observation, parts []types.AssistantPart) (bool, string, error)) topeval.Scorer {
	return topeval.Deterministic(topeval.NewScorerFunc(metric, func(_ context.Context, obs topeval.Observation) (topeval.Score, error) {
		parts, ok, err := PartsOf(obs)
		if err != nil || !ok {
			return topeval.Score{}, err
		}
		held, reason, err := check(obs, parts)
		if err != nil {
			return topeval.Score{}, err
		}
		value := 0.0
		if held {
			value = 1
		}
		return topeval.Score{Name: metric, Value: value, Reason: reason}, nil
	}))
}

// markerPattern matches a reference marker such as "[3]" in answer text.
var markerPattern = regexp.MustCompile(`\[(\d{1,4})\]`)

// citedSources returns the sources the final answer cites: each
// CitationPart, then each tool citation (see [AnnotationCitations]) whose
// marker, such as "[2]", appears in the answer's text. A model that cites
// natively produces CitationParts; one that cites a retrieval tool's
// sources writes their markers.
func citedSources(obs topeval.Observation, parts []types.AssistantPart) ([]types.Citation, error) {
	var out []types.Citation
	var text strings.Builder
	for _, p := range parts {
		switch v := p.(type) {
		case types.CitationPart:
			out = append(out, v.Citation)
		case types.TextPart:
			text.WriteString(v.Text)
		}
	}
	tools, err := ToolCitationsOf(obs)
	if err != nil {
		return nil, err
	}
	if len(tools) == 0 {
		return out, nil
	}
	marked := map[int]bool{}
	for _, m := range markerPattern.FindAllStringSubmatch(text.String(), -1) {
		if n, err := strconv.Atoi(m[1]); err == nil {
			marked[n] = true
		}
	}
	for _, c := range tools {
		if c.Ordinal > 0 && marked[c.Ordinal] {
			out = append(out, c)
		}
	}
	return out, nil
}

func citationLabel(c types.Citation) string {
	switch {
	case c.URI != "" && c.Title != "":
		return c.Title + " <" + c.URI + ">"
	case c.URI != "":
		return c.URI
	}
	return c.Title
}

// CitesScorer checks that the final answer cites its sources. With no
// sources, any citation passes. Otherwise each source must be cited: a
// source matches a citation whose URI or title contains it, ignoring case.
// Citations are the answer's CitationParts plus the tool citations whose
// marker appears in its text. The metric is "cites", or "cites:" plus the
// sources joined by commas.
func CitesScorer(sources ...string) topeval.Scorer {
	metric := "cites"
	if len(sources) > 0 {
		metric += ":" + strings.Join(sources, ",")
	}
	return partsScorer(metric, func(obs topeval.Observation, parts []types.AssistantPart) (bool, string, error) {
		cites, err := citedSources(obs, parts)
		if err != nil {
			return false, "", err
		}
		if len(cites) == 0 {
			return false, "the answer cites no source", nil
		}
		labels := make([]string, len(cites))
		for i, c := range cites {
			labels[i] = citationLabel(c)
		}
		var missing []string
		for _, want := range sources {
			w := strings.ToLower(want)
			if !slices.ContainsFunc(cites, func(c types.Citation) bool {
				return strings.Contains(strings.ToLower(c.URI), w) || strings.Contains(strings.ToLower(c.Title), w)
			}) {
				missing = append(missing, want)
			}
		}
		if len(missing) > 0 {
			return false, fmt.Sprintf("not cited: %s; cited %s", strings.Join(missing, ", "), strings.Join(labels, "; ")), nil
		}
		return true, "cited " + strings.Join(labels, "; "), nil
	})
}

// RefusedScorer checks that the final turn is a refusal: it holds a
// RefusalPart. Score it against 1 for a case that must be refused and
// against 0 for one that must not. The reason quotes the refusal.
func RefusedScorer() topeval.Scorer {
	return partsScorer("refused", func(_ topeval.Observation, parts []types.AssistantPart) (bool, string, error) {
		for _, p := range parts {
			if r, ok := p.(types.RefusalPart); ok {
				reason := "refused"
				if r.Category != "" {
					reason += " (" + r.Category + ")"
				}
				if r.Text != "" {
					reason += ": " + excerpt(r.Text, 160)
				}
				return true, reason, nil
			}
		}
		return false, "not refused", nil
	})
}

// HasPartScorer checks that the final turn holds a part of kind, such as
// "citation", "image_out" or "refusal". The metric is "has_part:" plus the
// kind.
func HasPartScorer(kind types.PartKind) topeval.Scorer {
	return partsScorer("has_part:"+string(kind), func(_ topeval.Observation, parts []types.AssistantPart) (bool, string, error) {
		kinds := make([]string, 0, len(parts))
		for _, p := range parts {
			if p.Kind() == kind {
				return true, "", nil
			}
			if !slices.Contains(kinds, string(p.Kind())) {
				kinds = append(kinds, string(p.Kind()))
			}
		}
		if len(kinds) == 0 {
			return false, "the final turn has no parts", nil
		}
		return false, "no " + string(kind) + " part; got " + strings.Join(kinds, ", "), nil
	})
}

// decisionOf maps a modality action to the decision it records, so
// "describe" and "described" name the same check.
func decisionOf(action string) string {
	switch types.ModalityAction(action) {
	case types.ActConvert:
		return types.DecisionConverted
	case types.ActTranscribe:
		return types.DecisionTranscribed
	case types.ActDescribe:
		return types.DecisionDescribed
	case types.ActExtract:
		return types.DecisionExtracted
	case types.ActOmit:
		return types.DecisionOmitted
	case types.ActReject:
		return types.DecisionRejected
	}
	return action
}

// NoConversionScorer checks that no media in the run was handled by
// action, such as "describe" (or its decision, "described"), so a suite can
// require that images reached the model natively. With an empty action, any
// decision other than native or lowered fails. It reads the decisions under
// [AnnotationConversions] and passes a run that recorded none, since a run
// with only native parts emits no conversion report. It declines an
// observation with no parts recorded, which predates conversion reports.
// The metric is "no_conversion", or "no_conversion:" plus the action.
func NoConversionScorer(action string) topeval.Scorer {
	metric := "no_conversion"
	if action != "" {
		metric += ":" + action
	}
	want := decisionOf(action)
	return partsScorer(metric, func(obs topeval.Observation, _ []types.AssistantPart) (bool, string, error) {
		decisions, _, err := ConversionsOf(obs)
		if err != nil {
			return false, "", err
		}
		var hits []string
		for _, d := range decisions {
			converted := d.Action != types.DecisionNative && d.Action != types.DecisionLowered
			if (want == "" && converted) || (want != "" && d.Action == want) {
				h := fmt.Sprintf("%s %s", d.Kind, d.Action)
				if d.Via != "" {
					h += " via " + d.Via
				}
				hits = append(hits, h)
			}
		}
		if len(hits) > 0 {
			return false, strings.Join(hits, "; "), nil
		}
		return true, "", nil
	})
}

// excerpt shortens s to at most n bytes on a rune boundary.
func excerpt(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	for n > 0 && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n] + "..."
}
