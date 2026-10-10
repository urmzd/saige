package online_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	agenteval "github.com/urmzd/saige/agent/eval"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/eval"
	"github.com/urmzd/saige/eval/online"
)

// mediaConversation is a run whose question carries an image, served with
// a recorded conversion, that ends with a cited answer.
func mediaConversation(t *testing.T) []turn {
	t.Helper()
	img := types.Artifact("saige-artifact://9f1c", types.MediaPNG)
	report := types.ConversionReport{Decisions: []types.ConversionDecision{
		{Kind: types.KindImage, MediaType: types.MediaPNG, Action: types.DecisionDescribed, Via: "describe@1"},
	}}
	return []turn{
		{types.UserMsg(types.Text("What does this receipt total?"), types.Image(img))},
		{types.AssistantMessage{Parts: []types.AssistantPart{
			types.RoutePart{Model: "gpt-6-luna", Conversions: &report},
			types.TextPart{Text: "It totals $12."},
			types.CitationPart{Citation: types.NewCitation(types.CitationDocument, "", "receipt")},
		}}},
	}
}

func TestFromPathReadsTypedParts(t *testing.T) {
	tr, ids := conversation(t, mediaConversation(t)...)
	recs, err := online.TreeSource{"c": tr}.Records(context.Background(), online.Window{})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("records = %d", len(recs))
	}
	rec := recs[0]
	if rec.Ref.Node != string(ids[1]) || rec.Input != "What does this receipt total?" || rec.Output != "It totals $12." {
		t.Fatalf("record = %+v", rec)
	}
	if len(rec.InputParts) != 2 || rec.InputParts[1].Kind() != types.KindImage {
		t.Fatalf("input parts = %#v", rec.InputParts)
	}
	if len(rec.Parts) != 2 || rec.Parts[1].Kind() != types.KindCitation {
		t.Fatalf("final parts = %#v (route parts are metadata)", rec.Parts)
	}
	if len(rec.Conversions) != 1 || rec.Conversions[0].Action != types.DecisionDescribed {
		t.Fatalf("conversions = %+v", rec.Conversions)
	}

	obs, err := rec.Observation()
	if err != nil {
		t.Fatal(err)
	}
	in, err := eval.DecodeInput(obs.Input)
	if err != nil {
		t.Fatal(err)
	}
	if in.Kind != eval.InputParts || in.Text() != rec.Input || len(in.Media()) != 1 {
		t.Fatalf("observation input = %s", obs.Input)
	}
	if src, _ := types.SourceOf(in.Media()[0]); src.Ref != "saige-artifact://9f1c" {
		t.Fatalf("media source = %+v", src)
	}
	cites, err := agenteval.CitesScorer("receipt").Score(context.Background(), obs)
	if err != nil || cites.Value != 1 {
		t.Fatalf("cites = %+v, %v", cites, err)
	}
	nc, err := agenteval.NoConversionScorer("describe").Score(context.Background(), obs)
	if err != nil || nc.Value != 0 {
		t.Fatalf("no_conversion = %+v, %v", nc, err)
	}
}

func TestTextRunObservationInputIsUnchanged(t *testing.T) {
	tr, _ := supportConversation(t)
	recs, err := online.TreeSource{"c": tr}.Records(context.Background(), online.Window{})
	if err != nil {
		t.Fatal(err)
	}
	obs, err := recs[0].Observation()
	if err != nil {
		t.Fatal(err)
	}
	var s string
	if err := json.Unmarshal(obs.Input, &s); err != nil || s != recs[0].Input {
		t.Fatalf("input = %s", obs.Input)
	}
	if _, ok := obs.Annotations[agenteval.AnnotationParts]; !ok {
		t.Fatal("a tree record has no parts annotation")
	}
	// A hand-built record, as callers wrote before parts, records none, and
	// the part scorers decline it.
	legacy, err := online.Record{Ref: online.Ref{Conversation: "c", Node: "n"}, Input: "q", Output: "a"}.Observation()
	if err != nil {
		t.Fatal(err)
	}
	if sc, _ := agenteval.RefusedScorer().Score(context.Background(), legacy); sc.Name != "" {
		t.Fatalf("scored a record without parts: %+v", sc)
	}
}

func TestPromotedMediaCaseKeepsReferences(t *testing.T) {
	tr, _ := conversation(t, mediaConversation(t)...)
	recs, _ := online.TreeSource{"c": tr}.Records(context.Background(), online.Window{})
	obs, err := recs[0].Observation()
	if err != nil {
		t.Fatal(err)
	}
	obs.Labels[online.LabelFlagged] = "true"
	cases, err := online.Promote(context.Background(), []eval.Unit{{RunID: "r", Observation: obs}}, online.PromoteOptions{})
	if err != nil || len(cases) != 1 {
		t.Fatalf("cases = %+v, %v", cases, err)
	}
	in, err := eval.DecodeInput(cases[0].Input)
	if err != nil || len(in.Media()) != 1 {
		t.Fatalf("promoted input = %s, %v", cases[0].Input, err)
	}
}

func TestBudgetedJudgeSeesInputMedia(t *testing.T) {
	prov := &agenttest.ScriptedProvider{Responses: [][]types.Delta{judgeResponse(1)}}
	gen := &online.BudgetedGenerator{Provider: prov, Budget: types.NewBudget(types.BudgetPolicy{MaxRequests: 1})}
	tr, _ := conversation(t, mediaConversation(t)...)
	recs, _ := online.TreeSource{"c": tr}.Records(context.Background(), online.Window{})
	obs, _ := recs[0].Observation()
	sc, err := eval.NewJudgeScorer(gen).Score(context.Background(), obs)
	if err != nil || sc.Value != 1 {
		t.Fatalf("score = %+v, %v", sc, err)
	}
	reqs := prov.Requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d", len(reqs))
	}
	msg := reqs[0].Messages[len(reqs[0].Messages)-1].(types.UserMessage)
	if len(msg.Parts) != 2 || msg.Parts[1].Kind() != types.KindImage {
		t.Fatalf("judge sent %#v", msg.Parts)
	}
}
