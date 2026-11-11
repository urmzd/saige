package native_test

import (
	"encoding/json"
	"testing"

	ragtypes "github.com/urmzd/saige/rag/types"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/turn/native"
)

// A citation event has to carry everything a footnote needs (the marker, the
// exact source text, and somewhere to go) or a reader cannot check the claim.
func TestCitationsCarryTheQuotedTextLabelAndProvenance(t *testing.T) {
	t.Parallel()

	ac := &ragtypes.AssembledContext{
		Prompt: "Context for query \"deploys\":\n\n[1] ...",
		Blocks: []ragtypes.ContextBlock{
			{
				Text:     "Deploys run on merge to main.",
				Citation: "[1]",
				Provenance: ragtypes.Provenance{
					DocumentUUID:   "doc-1",
					DocumentTitle:  "Runbook",
					SourceURI:      "https://example.test/runbook#deploys",
					SectionUUID:    "sec-3",
					SectionHeading: "Deploys",
					SectionIndex:   3,
				},
			},
			{
				Text:       "Rollbacks are manual.",
				Citation:   "[2]",
				Provenance: ragtypes.Provenance{DocumentUUID: "doc-2"},
			},
		},
	}

	got := native.Citations(ac)
	if len(got) != 2 {
		t.Fatalf("got %d citation events, want 2", len(got))
	}

	first := decodeCitation(t, got[0])
	if first.Label != "[1]" {
		t.Errorf("label is %q, want %q", first.Label, "[1]")
	}
	if first.Text != "Deploys run on merge to main." {
		t.Errorf("text is %q, want the exact source text", first.Text)
	}
	if first.Index != 0 {
		t.Errorf("index is %d, want 0", first.Index)
	}
	want := event.Provenance{
		DocumentUUID: "doc-1", DocumentTitle: "Runbook",
		SourceURI:   "https://example.test/runbook#deploys",
		SectionUUID: "sec-3", SectionHeading: "Deploys", SectionIndex: 3,
	}
	if first.Source != want {
		t.Errorf("provenance is %+v, want %+v", first.Source, want)
	}

	second := decodeCitation(t, got[1])
	if second.Index != 1 {
		t.Errorf("second index is %d, want 1", second.Index)
	}
	for _, c := range got {
		if c.Type() != event.TypeCitation {
			t.Errorf("kind is %s, want %s", c.Type(), event.TypeCitation)
		}
	}
}

// An assembler that emits no inline marker still has to produce renderable
// footnotes, so citations are numbered by position when unlabelled.
func TestUnlabelledCitationsAreNumberedByPosition(t *testing.T) {
	t.Parallel()

	ac := &ragtypes.AssembledContext{Blocks: []ragtypes.ContextBlock{
		{Text: "first"},
		{Text: "second"},
	}}
	got := native.Citations(ac)
	if len(got) != 2 {
		t.Fatalf("got %d citation events, want 2", len(got))
	}
	for i, want := range []string{"[1]", "[2]"} {
		if label := decodeCitation(t, got[i]).Label; label != want {
			t.Errorf("citation %d is labelled %q, want %q", i, label, want)
		}
	}
}

// Nothing to cite must produce nothing, and a block with neither text nor a
// label would render as an empty footnote.
func TestEmptyRetrievalProducesNoCitations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   *ragtypes.AssembledContext
	}{
		{"no context at all", nil},
		{"no blocks", &ragtypes.AssembledContext{Prompt: "nothing found"}},
		{"a blank block", &ragtypes.AssembledContext{Blocks: []ragtypes.ContextBlock{{Text: "   "}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := native.Citations(tc.in); len(got) != 0 {
				t.Fatalf("got %d citation events, want none", len(got))
			}
		})
	}
}

func decodeCitation(t *testing.T, c event.Body) event.Citation {
	t.Helper()
	var out event.Citation
	if err := json.Unmarshal(raw(c), &out); err != nil {
		t.Fatalf("decode citation: %v", err)
	}
	return out
}
