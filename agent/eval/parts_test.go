package eval

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/types"
	topeval "github.com/urmzd/saige/eval"
)

func partFeed(deltas ...types.Delta) <-chan types.Delta {
	ch := make(chan types.Delta, len(deltas))
	for _, d := range deltas {
		ch <- d
	}
	close(ch)
	return ch
}

func turn(parts ...types.AssistantPart) []types.Delta {
	var out []types.Delta
	for i, p := range parts {
		out = append(out, types.PartDeltas(i, p)...)
	}
	return append(out, types.UsageDelta{PromptTokens: 10, CompletionTokens: 5})
}

// ragRun streams a two-call run: a retrieval tool call that cites a source,
// with the image of the question extracted to text, then an answer that
// cites the source by marker and natively.
func ragRun() []types.Delta {
	report := types.ConversionReport{Decisions: []types.ConversionDecision{
		{Kind: types.KindImage, MediaType: types.MediaPNG, Action: types.DecisionNative},
		{Kind: types.KindDocument, MediaType: types.MediaPDF, Action: types.DecisionExtracted, Via: "documents@1"},
	}}
	var ds []types.Delta
	ds = append(ds, types.RouteDelta{Model: "m", Conversions: &report}, types.ConversionDelta{Report: report})
	ds = append(ds, turn(types.ToolCallPart{ID: "c1", Name: "rag_search", Arguments: map[string]any{"query": "q"}})...)
	ds = append(ds,
		types.ToolExecStartDelta{ToolCallID: "c1", Name: "rag_search"},
		types.CitationDelta{ToolCallID: "c1", Citation: types.Citation{Ordinal: 1, Kind: types.CitationRetrieval,
			URI: "file://handbook.pdf", Title: "Handbook", Start: -1, End: -1}},
		types.CitationDelta{ToolCallID: "c1", Citation: types.Citation{Ordinal: 2, Kind: types.CitationRetrieval,
			URI: "file://unused.pdf", Start: -1, End: -1}},
		types.ToolExecEndDelta{ToolCallID: "c1", Name: "rag_search", Result: "[]"},
	)
	ds = append(ds, turn(
		types.TextPart{Text: "Refunds take 30 days [1]."},
		types.CitationPart{Citation: types.NewCitation(types.CitationDocument, "", "Policy"), Anchor: &types.Anchor{PartIndex: 0, Start: 0, End: 7}},
		types.ImageOutPart{Source: types.Artifact("saige-artifact://abc", types.MediaPNG)},
	)...)
	return ds
}

func TestCollectAgentRunParts(t *testing.T) {
	run := CollectAgentRun(partFeed(ragRun()...))
	if len(run.Parts) != 3 {
		t.Fatalf("final parts = %#v", run.Parts)
	}
	if _, ok := run.Parts[1].(types.CitationPart); !ok {
		t.Fatalf("part 1 = %T", run.Parts[1])
	}
	if len(run.Media) != 1 || run.Media[0].Kind != types.KindImageOut || run.Media[0].Ref != "saige-artifact://abc" {
		t.Fatalf("media = %+v", run.Media)
	}
	if len(run.Conversions) != 2 || run.Conversions[1].Via != "documents@1" {
		t.Fatalf("conversions = %+v", run.Conversions)
	}
	if len(run.Routes) != 1 || run.Routes[0].Conversions == nil {
		t.Fatalf("routes = %+v", run.Routes)
	}
	if len(run.Citations) != 2 {
		t.Fatalf("citations = %+v", run.Citations)
	}

	var p topeval.Provenance
	run.AddProvenance(&p)
	if len(p.Conversions) != 2 {
		t.Fatalf("provenance conversions = %+v", p.Conversions)
	}
}

func TestCollectAgentRunPartsOfAFailedCall(t *testing.T) {
	ds := turn(types.TextPart{Text: "first"})
	ds = append(ds, types.PartStart{Index: 0, Kind: types.KindText}, types.PartDelta{Index: 0, Text: "cut sh"},
		types.ErrorDelta{Error: context.DeadlineExceeded})
	run := CollectAgentRun(partFeed(ds...))
	if len(run.Parts) != 1 || types.TextOf(types.AssistantMsg(run.Parts...)) != "cut sh" {
		t.Fatalf("parts = %#v", run.Parts)
	}
	if empty := CollectAgentRun(partFeed()); empty.Parts == nil || len(empty.Parts) != 0 {
		t.Fatalf("an empty run has parts %#v", empty.Parts)
	}
}

func annotated(t *testing.T, ds []types.Delta) topeval.Observation {
	t.Helper()
	obs := topeval.Observation{ID: "o"}
	if err := AnnotateObservation(&obs, CollectAgentRun(partFeed(ds...))); err != nil {
		t.Fatal(err)
	}
	return obs
}

func TestAnnotateObservationRecordsParts(t *testing.T) {
	obs := annotated(t, ragRun())
	parts, ok, err := PartsOf(obs)
	if err != nil || !ok || len(parts) != 3 {
		t.Fatalf("parts = %v, %v, %v", parts, ok, err)
	}
	for _, key := range []string{AnnotationMedia, AnnotationConversions, AnnotationCitations} {
		if _, ok := obs.Annotations[key]; !ok {
			t.Errorf("missing %s", key)
		}
	}
	// Bytes never reach an annotation.
	src := types.Bytes(types.MediaPNG, []byte("raw image bytes"))
	obs = annotated(t, turn(types.ImageOutPart{Source: src}))
	if strings.Contains(string(obs.Annotations[AnnotationParts]), "data") {
		t.Fatalf("bytes in parts annotation: %s", obs.Annotations[AnnotationParts])
	}
	// A run without conversions or tool citations does not record them.
	obs = annotated(t, turn(types.TextPart{Text: "hi"}))
	if _, ok := obs.Annotations[AnnotationConversions]; ok {
		t.Fatal("empty conversions recorded")
	}
	if _, ok := obs.Annotations[AnnotationParts]; !ok {
		t.Fatal("parts not recorded for a text run")
	}
}

func score(t *testing.T, s topeval.Scorer, obs topeval.Observation) topeval.Score {
	t.Helper()
	sc, err := s.Score(context.Background(), obs)
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

func TestPartScorers(t *testing.T) {
	rag := annotated(t, ragRun())
	plain := annotated(t, turn(types.TextPart{Text: "No sources here [3]."}))
	refusal := annotated(t, turn(types.RefusalPart{Text: "I can't help with that.", Category: "safety"}))

	tests := []struct {
		name   string
		scorer topeval.Scorer
		obs    topeval.Observation
		want   float64
		reason string
	}{
		{"cites any", CitesScorer(), rag, 1, "Handbook <file://handbook.pdf>"},
		{"cites marker and part", CitesScorer("handbook", "policy"), rag, 1, ""},
		{"unmarked tool source is not cited", CitesScorer("unused"), rag, 0, "not cited: unused"},
		{"an unknown marker is not a citation", CitesScorer(), plain, 0, "cites no source"},
		{"refused", RefusedScorer(), refusal, 1, "refused (safety): I can't help with that."},
		{"not refused", RefusedScorer(), rag, 0, "not refused"},
		{"has citation", HasPartScorer(types.KindCitation), rag, 1, ""},
		{"has no refusal", HasPartScorer(types.KindRefusal), rag, 0, "got text, citation, image_out"},
		{"no describe", NoConversionScorer("describe"), rag, 1, ""},
		{"extracted", NoConversionScorer("extract"), rag, 0, "document extracted via documents@1"},
		{"any conversion", NoConversionScorer(""), rag, 0, "document extracted"},
		{"native only", NoConversionScorer(""), plain, 1, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := score(t, tt.scorer, tt.obs)
			if sc.Value != tt.want || !strings.Contains(sc.Reason, tt.reason) {
				t.Fatalf("score = %+v, want %g with %q", sc, tt.want, tt.reason)
			}
		})
	}
}

// legacyObservation is the unit a v0.32 run stored, before parts existed.
func legacyObservation(t *testing.T) topeval.Observation {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "eval", "testdata", "legacy-v0.32", "store", "run-legacy", "units.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var u topeval.Unit
	if err := json.Unmarshal([]byte(strings.SplitN(string(data), "\n", 2)[0]), &u); err != nil {
		t.Fatal(err)
	}
	return u.Observation
}

func TestScorersOnLegacyObservations(t *testing.T) {
	obs := legacyObservation(t)
	for _, s := range []topeval.Scorer{CitesScorer(), RefusedScorer(), HasPartScorer(types.KindText), NoConversionScorer("")} {
		if sc := score(t, s, obs); sc.Name != "" {
			t.Errorf("%s scored a legacy observation: %+v", s.Name(), sc)
		}
	}
	if sc := score(t, ToolCallCountScorer(), obs); sc.Value != 1 {
		t.Fatalf("tool_call_count = %+v", sc)
	}
	if sc := score(t, CallsWithScorer("rag_search", map[string]any{"query": "capital of France"}), obs); sc.Value != 1 {
		t.Fatalf("calls_with = %+v", sc)
	}
}

func TestRegistryBuildsPartScorers(t *testing.T) {
	r := topeval.NewRegistry()
	if err := RegisterScorers(r); err != nil {
		t.Fatal(err)
	}
	specs := []topeval.ScorerSpec{
		{Kind: "cites", Params: json.RawMessage(`{"sources":["handbook"]}`)},
		{Kind: "cites"},
		{Kind: "refused"},
		{Kind: "has_part", Params: json.RawMessage(`{"kind":"citation"}`)},
		{Kind: "no_conversion", Params: json.RawMessage(`{"action":"describe"}`)},
	}
	want := []string{"cites:handbook", "cites", "refused", "has_part:citation", "no_conversion:describe"}
	for i, spec := range specs {
		s, err := r.Build(spec)
		if err != nil {
			t.Fatalf("%s: %v", spec.Kind, err)
		}
		if s.Name() != want[i] {
			t.Errorf("name = %q, want %q", s.Name(), want[i])
		}
	}
	if _, err := r.Build(topeval.ScorerSpec{Kind: "has_part"}); err == nil {
		t.Fatal("has_part without a kind built")
	}
}
