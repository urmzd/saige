package native_test

import (
	"context"
	"slices"
	"testing"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	saigetypes "github.com/urmzd/saige/agent/types"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/turn/native"
)

// citingSearch is a tool whose result cites one web source.
type citingSearch struct{ uri, title string }

func (citingSearch) Definition() saigetypes.ToolDef {
	return saigetypes.ToolDef{Name: "search", Parameters: saigetypes.ParameterSchema{Type: "object"}}
}

func (s citingSearch) Execute(ctx context.Context, args map[string]any) (string, error) {
	r, err := s.ExecuteRich(ctx, args)
	return r.Text(), err
}

func (s citingSearch) ExecuteRich(context.Context, map[string]any) (saigetypes.ToolResult, error) {
	return saigetypes.ToolResult{
		Parts:     []saigetypes.ToolOutputPart{saigetypes.Text("passage")},
		Citations: []saigetypes.Citation{saigetypes.NewCitation(saigetypes.CitationWeb, s.uri, s.title)},
	}, nil
}

// A source a sub-agent's tool cites is published as a citation under the
// delegating call: the child's label, with Parent naming the delegation.
// flow/connectors/internal/render matches this shape (see its cite tests).
func TestTranslatorPublishesASubAgentsCitationUnderItsDelegation(t *testing.T) {
	parent := &agenttest.ScriptedProvider{Responses: [][]saigetypes.Delta{
		agenttest.ToolCallResponse("d1", "delegate_to_researcher", map[string]any{"task": "find"}),
		agenttest.TextResponse("The researcher found it [1]."),
	}}
	child := &agenttest.ScriptedProvider{Responses: [][]saigetypes.Delta{
		agenttest.ToolCallResponse("s1", "search", map[string]any{}),
		agenttest.TextResponse("Found [1]."),
	}}
	a, err := agent.New(agent.Config{
		Name:     "lead",
		Provider: parent,
		SubAgents: []agent.SubAgentDef{{
			Name: "researcher", Provider: child,
			Tools: saigetypes.NewToolRegistry(citingSearch{uri: "https://example.com/a", title: "Alpha"}),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	deltas := agenttest.CollectDeltas(a.Invoke(context.Background(), []saigetypes.Message{saigetypes.UserMsg(saigetypes.Text("go"))}).Deltas())

	tr := native.NewTranslator()
	var cites []event.Citation
	for _, d := range deltas {
		if b, ok := tr.Delta(d); ok {
			if c, ok := b.(event.Citation); ok {
				cites = append(cites, c)
			}
		}
	}
	if len(cites) != 1 {
		t.Fatalf("citations = %+v, want one", cites)
	}
	c := cites[0]
	if c.Label != "[1]" || !slices.Equal(c.Parent, []string{"d1"}) || c.ToolCallID != "s1" ||
		c.Source.SourceURI != "https://example.com/a" || c.Source.DocumentTitle != "Alpha" {
		t.Errorf("citation = %+v, want label [1] under d1 from s1 for Alpha", c)
	}
}

// Each sub-agent numbers its own sources, and a forwarded citation keeps the
// child's label: two sibling delegations that each cite one source both
// publish "[1]", under their own delegation.
func TestTranslatorKeepsEachSubAgentsOwnLabel(t *testing.T) {
	parent := &agenttest.ScriptedProvider{Responses: [][]saigetypes.Delta{
		agenttest.ToolCallResponse("d1", "delegate_to_r1", map[string]any{"task": "find"}),
		agenttest.ToolCallResponse("d2", "delegate_to_r2", map[string]any{"task": "find"}),
		agenttest.TextResponse("A [1] and B [2]."),
	}}
	childOf := func() *agenttest.ScriptedProvider {
		return &agenttest.ScriptedProvider{Responses: [][]saigetypes.Delta{
			agenttest.ToolCallResponse("s1", "search", map[string]any{}),
			agenttest.TextResponse("Found [1]."),
		}}
	}
	a, err := agent.New(agent.Config{
		Name:     "lead",
		Provider: parent,
		SubAgents: []agent.SubAgentDef{
			{Name: "r1", Provider: childOf(), Tools: saigetypes.NewToolRegistry(citingSearch{uri: "https://example.com/a", title: "A"})},
			{Name: "r2", Provider: childOf(), Tools: saigetypes.NewToolRegistry(citingSearch{uri: "https://example.com/b", title: "B"})},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	deltas := agenttest.CollectDeltas(a.Invoke(context.Background(), []saigetypes.Message{saigetypes.UserMsg(saigetypes.Text("go"))}).Deltas())

	tr := native.NewTranslator()
	got := map[string]string{}
	for _, d := range deltas {
		if b, ok := tr.Delta(d); ok {
			if c, ok := b.(event.Citation); ok {
				got[c.Source.DocumentTitle] = c.Label + " " + c.Parent[0]
			}
		}
	}
	if got["A"] != "[1] d1" || got["B"] != "[1] d2" || len(got) != 2 {
		t.Errorf("citations = %v, want A as [1] under d1 and B as [1] under d2", got)
	}
}
