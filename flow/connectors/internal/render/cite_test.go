package render_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/urmzd/saige/flow/connectors/internal/render"
	"github.com/urmzd/saige/flow/event"
)

func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func TestCitedLabelsReadsBracketedReferencesOutsideCode(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []string
	}{
		{"none", "No sources here.", []string{}},
		{"one", "The bus is durable [2].", []string{"2"}},
		{"adjacent", "Both [1][3].", []string{"1", "3"}},
		{"list", "Both [1, 3; 4].", []string{"1", "3", "4"}},
		{"footnote form", "Durable[^2].", []string{"2"}},
		{"double brackets", "Durable [[2]].", []string{"2"}},
		{"word label", "Per [rfc].", []string{"rfc"}},
		{"quotation", `It says "the bus is durable [2]".`, []string{"2"}},
		{"blockquote", "> the bus is durable [2]\n\nSo it is.", []string{"2"}},
		{"inline code", "Use `xs[1]` to read it.", []string{}},
		{"double backtick span", "Use ``a`[1]`` here.", []string{}},
		{"unclosed backtick", "A ` then [2].", []string{"2"}},
		{"fenced code", "Example:\n```go\nx := xs[1]\n```\nDone [3].", []string{"3"}},
		{"tilde fence", "~~~\n[1]\n~~~\n", []string{}},
		{"unclosed fence", "```\n[1]\n", []string{}},
		{"link text", "[docs](https://example.com) and [2]", []string{"2", "docs"}},
		{"across lines", "[1\n] and [2]", []string{"2"}},
		{"too long", "[" + strings.Repeat("a", 100) + "]", []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := keys(render.CitedLabels(tc.text)); !slices.Equal(got, tc.want) {
				t.Errorf("CitedLabels(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

func TestLabelKeyStripsBracketsAndFootnoteMarker(t *testing.T) {
	for in, want := range map[string]string{"[1]": "1", " 1 ": "1", "[^1]": "1", "rfc": "rfc", "[]": ""} {
		if got := render.LabelKey(in); got != want {
			t.Errorf("LabelKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func citation(seq uint64, label, title string) event.Envelope {
	return ev(seq, event.TypeCitation, fmt.Sprintf(`{"label":%q,"text":"passage %s","source":{"document_title":%q,"source_uri":"https://example.com/%s"}}`, label, title, title, title))
}

func TestViewListsOnlyTheSourcesTheAnswerCites(t *testing.T) {
	d := render.New()
	d.Apply(citation(1, "[1]", "alpha"))
	d.Apply(citation(2, "[2]", "beta"))
	d.Apply(citation(3, "[3]", "gamma"))
	if got := d.View().Citations; len(got) != 0 {
		t.Fatalf("before any answer, citations = %+v", got)
	}

	d.Apply(text(4, "Beta says so [2]."))
	got := d.View().Citations
	if len(got) != 1 || got[0].Title != "beta" || got[0].Ref() != "2" || got[0].N != 1 {
		t.Fatalf("citations = %+v, want only beta", got)
	}

	d.Apply(ev(5, event.TypeCompleted, `{"text":"No sources were needed."}`))
	if got := d.View().Citations; len(got) != 0 {
		t.Errorf("an answer citing nothing lists %+v", got)
	}
}

func TestViewJoinsTheCitedLabelsOfOneSource(t *testing.T) {
	d := render.New()
	d.Apply(citation(1, "[1]", "alpha"))
	d.Apply(citation(2, "[2]", "alpha"))
	d.Apply(citation(3, "[3]", "alpha"))
	d.Apply(text(4, "See [3] and [1]."))
	got := d.View().Citations
	if len(got) != 1 || got[0].Ref() != "1, 3" {
		t.Fatalf("citations = %+v, want one footnote referred to as 1, 3", got)
	}
}

func TestViewIgnoresLabelsInCode(t *testing.T) {
	d := render.New()
	d.Apply(citation(1, "[1]", "alpha"))
	d.Apply(text(2, "Read it with `xs[1]`:\n```\nxs[1]\n```\n"))
	if got := d.View().Citations; len(got) != 0 {
		t.Errorf("citations = %+v, want none", got)
	}
}

func TestDocumentBoundsTheSourcesAndLabelsItKeeps(t *testing.T) {
	d := render.New()
	var answer strings.Builder
	for i := 1; i <= render.MaxSources+10; i++ {
		d.Apply(citation(uint64(i), fmt.Sprintf("[%d]", i), fmt.Sprintf("doc%d", i)))
		fmt.Fprintf(&answer, "[%d] ", i)
	}
	for i := 1; i <= render.MaxLabelsPerSource+10; i++ {
		d.Apply(citation(uint64(1000+i), fmt.Sprintf("[x%d]", i), "doc1"))
	}
	d.Apply(ev(5000, event.TypeCompleted, fmt.Sprintf(`{"text":%q}`, answer.String())))
	got := d.View().Citations
	if len(got) != render.MaxSources {
		t.Errorf("kept %d sources, want %d", len(got), render.MaxSources)
	}
	if len(got[0].Labels) != render.MaxLabelsPerSource {
		t.Errorf("kept %d labels for one source, want %d", len(got[0].Labels), render.MaxLabelsPerSource)
	}
}

func nestedCitation(seq uint64, label, title string) event.Envelope {
	return ev(seq, event.TypeCitation, fmt.Sprintf(`{"label":%q,"text":"passage %s","source":{"document_title":%q,"source_uri":"https://example.com/%s"},"parent":["call_1"]}`, label, title, title, title))
}

func TestViewListsANestedAgentsSourceTheAnswerCites(t *testing.T) {
	d := render.New()
	d.Apply(nestedCitation(1, "[1]", "alpha"))
	d.Apply(text(2, "The researcher found it [1]."))
	got := d.View().Citations
	if len(got) != 1 || got[0].Title != "alpha" || got[0].Ref() != "1" {
		t.Fatalf("citations = %+v, want alpha referred to as 1", got)
	}
}

func TestViewPrefersTheRunAgentsLabelOverANestedOne(t *testing.T) {
	d := render.New()
	d.Apply(nestedCitation(1, "[1]", "nested"))
	d.Apply(citation(2, "[1]", "alpha"))
	d.Apply(nestedCitation(3, "[2]", "beta"))
	d.Apply(text(4, "Alpha says so [1], and beta [2]."))
	got := d.View().Citations
	if len(got) != 2 || got[0].Title != "alpha" || got[0].Ref() != "1" || got[1].Title != "beta" || got[1].Ref() != "2" {
		t.Fatalf("citations = %+v, want alpha as 1 and beta as 2", got)
	}
}

func TestViewJoinsANestedLabelOfARunAgentsSource(t *testing.T) {
	d := render.New()
	d.Apply(citation(1, "[1]", "alpha"))
	d.Apply(nestedCitation(2, "[3]", "alpha"))
	d.Apply(text(3, "See [1] and [3]."))
	got := d.View().Citations
	if len(got) != 1 || got[0].Ref() != "1, 3" {
		t.Fatalf("citations = %+v, want alpha referred to as 1, 3", got)
	}
}
