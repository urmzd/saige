package render_test

import (
	"context"
	"testing"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/flow/connectors/internal/render"
	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/turn/native"
)

// citingSearch is a tool whose result cites one web source.
type citingSearch struct{}

func (citingSearch) Definition() types.ToolDef {
	return types.ToolDef{Name: "search", Parameters: types.ParameterSchema{Type: "object"}}
}

func (s citingSearch) Execute(ctx context.Context, args map[string]any) (string, error) {
	r, err := s.ExecuteRich(ctx, args)
	return r.Text(), err
}

func (citingSearch) ExecuteRich(context.Context, map[string]any) (types.ToolResult, error) {
	return types.ToolResult{
		Parts:     []types.ToolOutputPart{types.Text("passage")},
		Citations: []types.Citation{types.NewCitation(types.CitationWeb, "https://example.com/a", "Alpha")},
	}, nil
}

// An answer that cites a source a sub-agent found lists it: the events run
// from the agent loop through the native translator into the document.
func TestViewListsASubAgentsSourceTheAnswerCites(t *testing.T) {
	parent := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("d1", "delegate_to_researcher", map[string]any{"task": "find"}),
		agenttest.TextResponse("The researcher found it [1]."),
	}}
	child := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("s1", "search", map[string]any{}),
		agenttest.TextResponse("Found [1]."),
	}}
	a, err := agent.New(agent.Config{
		Name:      "lead",
		Provider:  parent,
		SubAgents: []agent.SubAgentDef{{Name: "researcher", Provider: child, Tools: types.NewToolRegistry(citingSearch{})}},
	})
	if err != nil {
		t.Fatal(err)
	}
	deltas := agenttest.CollectDeltas(a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("go"))}).Deltas())

	tr := native.NewTranslator()
	d := render.New()
	var seq uint64
	var answer string
	nested := 0
	for _, x := range deltas {
		b, ok := tr.Delta(x)
		if !ok {
			continue
		}
		if c, ok := b.(event.Citation); ok && len(c.Parent) > 0 {
			nested++
		}
		if tx, ok := b.(event.Text); ok && len(tx.Parent) == 0 {
			answer += tx.Text
		}
		seq++
		d.Apply(event.Envelope{Seq: seq, Data: b})
	}
	if nested == 0 {
		t.Fatal("the sub-agent's citation did not reach the document as a nested citation")
	}
	seq++
	d.Apply(event.Envelope{Seq: seq, Data: event.Completed{Text: answer}})

	got := d.View().Citations
	if len(got) != 1 || got[0].Title != "Alpha" || got[0].URI != "https://example.com/a" || got[0].Ref() != "1" {
		t.Fatalf("citations = %+v, want Alpha referred to as 1", got)
	}
}
