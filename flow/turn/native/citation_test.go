package native_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	saigetypes "github.com/urmzd/saige/agent/types"
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

// citingTool attributes its result to the sources it was made with.
type citingTool struct{ cites []saigetypes.Citation }

func (citingTool) Definition() saigetypes.ToolDef {
	return saigetypes.ToolDef{Name: "search", Parameters: saigetypes.ParameterSchema{Type: "object"}}
}

func (citingTool) Execute(context.Context, map[string]any) (string, error) { return "found", nil }

func (c citingTool) ExecuteRich(context.Context, map[string]any) (saigetypes.ToolResult, error) {
	return saigetypes.ToolResult{Parts: []saigetypes.ToolOutputPart{saigetypes.Text("found")}, Citations: c.cites}, nil
}

// A tool can cite a source the turn's retrieval already published. When it
// cites it under the same marker, the retrieved citation stands and the
// tool's is not published again; a source retrieval did not give the turn is
// published, with the call that cited it.
func TestAToolsCitationOfARetrievedSourceIsNotPublishedTwice(t *testing.T) {
	t.Parallel()

	const runbook = "https://example.test/runbook"
	rev := offlineRevision(1, "answer from the runbook")
	rev.Spec.Sources = []native.Source{{Name: "runbook", URI: "file://runbook.md"}}
	rev.Spec.Tools = []string{"search"}
	r, err := native.New(rev,
		native.WithTools(native.StaticTools(citingTool{cites: []saigetypes.Citation{
			saigetypes.NewCitation(saigetypes.CitationRetrieval, runbook, "Runbook"),
			saigetypes.NewCitation(saigetypes.CitationWeb, "https://example.test/blog", "Blog"),
		}})),
		native.WithRetriever(retrieverFunc(func(context.Context, string) (*ragtypes.AssembledContext, error) {
			return &ragtypes.AssembledContext{
				Prompt: "Context: deploys run on merge to main.",
				Blocks: []ragtypes.ContextBlock{{
					Text: "deploys run on merge to main", Citation: "[1]",
					Provenance: ragtypes.Provenance{DocumentTitle: "Runbook", SourceURI: runbook},
				}},
			}, nil
		})),
		native.WithScript(
			agenttest.ToolCallResponse("c1", "search", map[string]any{}),
			agenttest.TextResponse("deploys happen on merge [1], says the blog [2]"),
		))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, _ := collect(t, r, native.Turn{Text: "when do deploys happen?"})

	var cites []event.Citation
	for _, b := range got {
		if b.Type() == event.TypeCitation {
			cites = append(cites, decodeCitation(t, b))
		}
	}
	if len(cites) != 2 {
		t.Fatalf("got %d citations (%+v), want the retrieved one and the blog", len(cites), cites)
	}
	if c := cites[0]; c.Label != "[1]" || c.Source.SourceURI != runbook || c.ToolCallID != "" || c.Text != "deploys run on merge to main" {
		t.Errorf("first citation is %+v, want the retrieved passage", c)
	}
	if c := cites[1]; c.Label != "[2]" || c.Source.SourceURI != "https://example.test/blog" || c.ToolCallID != "c1" {
		t.Errorf("second citation is %+v, want the blog as [2] from c1", c)
	}
}
