package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

// TestToolCitationsReachTheEndAndTheResult checks that a rich tool's
// citations, numbered by the run's registry, are on its ToolExecEndDelta
// and on the ToolResultPart the model is sent and the tree keeps.
func TestToolCitationsReachTheEndAndTheResult(t *testing.T) {
	cite := types.NewCitation(types.CitationWeb, "https://example.com/a", "A")
	tool := &citingTool{cites: []types.Citation{cite}}
	provider := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("c1", "cite", map[string]any{}),
		agenttest.TextResponse("done"),
	}}
	a := must.Get(New(Config{Name: "a", Provider: provider, Tools: types.NewToolRegistry(tool)}))
	deltas := agenttest.CollectDeltas(a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("go"))}).Deltas())
	agenttest.AssertNoErrors(t, deltas)

	var end *types.ToolExecEndDelta
	for _, d := range deltas {
		if e, ok := d.(types.ToolExecEndDelta); ok {
			end = &e
		}
	}
	if end == nil || len(end.Citations) != 1 || end.Citations[0].URI != cite.URI || end.Citations[0].Ordinal != 1 {
		t.Fatalf("end = %+v, want the numbered citation", end)
	}
	if len(provider.Calls) != 2 {
		t.Fatalf("calls = %d", len(provider.Calls))
	}
	if r := citedResult(provider.Calls[1].Messages, "c1"); r == nil || len(r.Citations) != 1 || r.Citations[0].Ordinal != 1 {
		t.Fatalf("result sent = %+v, want its citation", r)
	}
	msgs, _ := a.Tree().FlattenBranch("main")
	if r := citedResult(msgs, "c1"); r == nil || len(r.Citations) != 1 {
		t.Fatalf("stored result = %+v, want its citation", r)
	}
}

// TestToolCitationsSurviveReplay checks that a tool step served from the
// durable record still registers, streams and stores its citations.
func TestToolCitationsSurviveReplay(t *testing.T) {
	cite := types.NewCitation(types.CitationWeb, "https://example.com/a", "A")
	prov := &toolCallProvider{toolName: "cite", toolID: "call-1", toolArgs: map[string]any{}, response: "done"}
	runner := newRecordingRunner()
	runner.seed("tool-call-1", types.StepResult{Kind: types.StepKindTool, ToolCallID: "call-1", ToolResult: "recorded",
		ToolCitations: []types.Citation{cite}})
	tool := &citingTool{}
	a := must.Get(New(Config{Provider: prov, Tools: types.NewToolRegistry(tool), SystemPrompt: "s"}))
	if _, err := a.RunDurable(context.Background(), runner, []types.Message{types.UserMsg(types.Text("go"))}, ""); err != nil {
		t.Fatal(err)
	}
	if src := a.Citations().Sources(); len(src) != 1 || src[0].URI != cite.URI {
		t.Fatalf("registry = %+v, want the recorded citation", src)
	}
	msgs, _ := a.Tree().FlattenBranch("main")
	if r := citedResult(msgs, "call-1"); r == nil || len(r.Citations) != 1 || r.Citations[0].Ordinal != 1 {
		t.Fatalf("stored result = %+v, want the recorded citation", r)
	}
}

func citedResult(msgs []types.Message, callID string) *types.ToolResultPart {
	for _, m := range msgs {
		for _, p := range types.PartsOf(m) {
			if r, ok := p.(types.ToolResultPart); ok && r.CallID == callID {
				return &r
			}
		}
	}
	return nil
}

// TestToolCitationsShowTheModelTheirMarkers checks that the model is sent a
// cited tool result led by its sources and their markers, so it can cite
// them, while the tree keeps the result as the tool returned it.
func TestToolCitationsShowTheModelTheirMarkers(t *testing.T) {
	cites := []types.Citation{
		types.NewCitation(types.CitationWeb, "https://example.com/a", "A"),
		types.NewCitation(types.CitationRetrieval, "", "Notes"),
		types.NewCitation(types.CitationWeb, "https://example.com/a", "A"), // same source, one line
	}
	provider := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("c1", "cite", map[string]any{}),
		agenttest.TextResponse("done [1]"),
	}}
	a := must.Get(New(Config{Name: "a", Provider: provider, Tools: types.NewToolRegistry(&citingTool{cites: cites})}))
	agenttest.AssertNoErrors(t, agenttest.CollectDeltas(a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("go"))}).Deltas()))

	sent := citedResult(provider.Calls[1].Messages, "c1")
	if sent == nil || len(sent.Parts) != 2 {
		t.Fatalf("result sent = %+v, want the source list and the output", sent)
	}
	want := "Sources (cite with the marker):\n[1] A <https://example.com/a>\n[2] Notes"
	if got := sent.Text(); !strings.HasPrefix(got, want) {
		t.Fatalf("text sent = %q, want it led by %q", got, want)
	}
	if lead, ok := sent.Parts[0].(types.TextPart); !ok || lead.Text != want {
		t.Fatalf("first part = %+v, want %q", sent.Parts[0], want)
	}
	msgs, _ := a.Tree().FlattenBranch("main")
	stored := citedResult(msgs, "c1")
	if stored == nil || stored.Text() != "see the linked resource" {
		t.Fatalf("stored result = %+v, want the tool's own output", stored)
	}
}

// TestCiteToolSourcesLeavesUncitedMessages checks that a request without
// numbered tool citations is passed through unchanged.
func TestCiteToolSourcesLeavesUncitedMessages(t *testing.T) {
	msgs := []types.Message{
		types.UserMsg(types.Text("hi")),
		types.ToolResults(types.ToolResultPart{CallID: "c1", Parts: []types.ToolOutputPart{types.Text("ok")},
			Citations: []types.Citation{types.NewCitation(types.CitationWeb, "https://x", "X")}}), // unnumbered
	}
	got := citeToolSources(msgs)
	if &got[0] != &msgs[0] {
		t.Fatal("uncited messages were copied")
	}
	if r := citedResult(got, "c1"); r.Text() != "ok" {
		t.Fatalf("text = %q", r.Text())
	}
}
