package native_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	saigetypes "github.com/urmzd/saige/agent/types"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/turn/native"
)

// Every delta the mapping claims to carry must produce an event of the declared
// kind with a body a reader can decode. A gap here is an event kind that quietly
// never appears on the bus.
func TestEveryMappedDeltaBecomesItsKind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   saigetypes.Delta
		kind string
		want string // the expected payload JSON
	}{
		{
			name: "text content",
			in:   saigetypes.PartDelta{Text: "hello"},
			kind: event.TypeText,
			want: `{"text":"hello"}`,
		},
		{
			name: "thinking content",
			in:   saigetypes.PartDelta{Thinking: "weighing options"},
			kind: event.TypeThinking,
			want: `{"text":"weighing options"}`,
		},
		{
			name: "tool call start",
			in:   saigetypes.PartStart{Kind: saigetypes.KindToolCall, ID: "call_1", Name: "grep"},
			kind: event.TypeToolCall,
			want: `{"id":"call_1","name":"grep","phase":"requested"}`,
		},
		{
			name: "tool exec start",
			in:   saigetypes.ToolExecStartDelta{ToolCallID: "call_2", Name: "build"},
			kind: event.TypeToolCall,
			want: `{"id":"call_2","name":"build","phase":"executing"}`,
		},
		{
			name: "tool exec end",
			in:   saigetypes.ToolExecEndDelta{ToolCallID: "call_3", Name: "build", Result: "ok"},
			kind: event.TypeToolDone,
			want: `{"id":"call_3","name":"build","result":"ok"}`,
		},
		{
			name: "tool exec end with error",
			in:   saigetypes.ToolExecEndDelta{ToolCallID: "call_4", Result: "failed", Error: "exit 1"},
			kind: event.TypeToolDone,
			want: `{"id":"call_4","result":"failed","error":"exit 1"}`,
		},
		{
			name: "nested tool delta unwraps to its inner kind",
			in:   saigetypes.ToolExecDelta{ToolCallID: "call_5", Inner: saigetypes.PartDelta{Text: "from a sub-agent"}},
			kind: event.TypeText,
			want: `{"text":"from a sub-agent","parent":["call_5"]}`,
		},
		{
			name: "a tool's citation",
			in: saigetypes.CitationDelta{ToolCallID: "call_6", Citation: saigetypes.Citation{
				Ordinal: 2, Kind: saigetypes.CitationWeb, URI: "https://example.test/a", Title: "A",
				Quote: " quoted ", Start: -1, End: -1, Producer: "web_search", Meta: map[string]any{"rank": 1},
			}},
			kind: event.TypeCitation,
			want: `{"label":"[2]","text":"quoted","source":{"document_title":"A","source_uri":"https://example.test/a"},"index":1,"tool_call_id":"call_6"}`,
		},
		{
			// The arguments, the markers' metadata and the interrupt's
			// payload stay out: none of them went through ToolArguments.
			name: "a tool call waiting for a decision",
			in: saigetypes.MarkerDelta{
				ToolCallID: "call_7", ToolName: "deploy", Arguments: map[string]any{"env": "prod"},
				Markers: []saigetypes.Marker{{Kind: "human_approval", Message: "Deploys need sign-off", Meta: map[string]any{"team": "ops"}}},
				Interrupt: &saigetypes.Interrupt{
					ID: "approval/call_7", Kind: saigetypes.InterruptApproval, Payload: json.RawMessage(`{"env":"prod"}`),
					ExpiresAt: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC),
				},
			},
			kind: event.TypeToolMarker,
			want: `{"id":"call_7","name":"deploy","markers":[{"kind":"human_approval","message":"Deploys need sign-off"}],"interrupt":{"id":"approval/call_7","kind":"approval","expires_at":"2026-11-08T12:00:00Z"}}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := native.NewTranslator().Delta(tc.in)
			if !ok {
				t.Fatalf("%T produced no event", tc.in)
			}
			if got.Type() != tc.kind {
				t.Errorf("kind is %s, want %s", got.Type(), tc.kind)
			}
			if string(raw(got)) != tc.want {
				t.Errorf("payload is %s, want %s", raw(got), tc.want)
			}
		})
	}
}

// A finished tool must be reported by name, taken from the ToolExecEndDelta
// itself. Two calls finishing in either order must not be confused for each
// other, which a translator that guessed from earlier deltas could do.
func TestToolDoneReportsItsOwnName(t *testing.T) {
	t.Parallel()

	tr := native.NewTranslator()
	tr.Delta(saigetypes.ToolExecStartDelta{ToolCallID: "a", Name: "read"})
	tr.Delta(saigetypes.ToolExecStartDelta{ToolCallID: "b", Name: "write"})

	for _, end := range []saigetypes.ToolExecEndDelta{
		{ToolCallID: "b", Name: "write"},
		{ToolCallID: "a", Name: "read"},
	} {
		c, ok := tr.Delta(end)
		if !ok {
			t.Fatalf("call %s produced no done event", end.ToolCallID)
		}
		var body event.ToolDone
		if err := json.Unmarshal(raw(c), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.ID != end.ToolCallID || body.Name != end.Name {
			t.Errorf("call %s finished as %s/%q, want %q", end.ToolCallID, body.ID, body.Name, end.Name)
		}
	}
}

// The deltas that are framing, telemetry, or lifecycle must NOT become events:
// string is a published contract and this package does not get to invent
// members of it.
func TestFramingAndTelemetryDeltasProduceNoEvents(t *testing.T) {
	t.Parallel()

	dropped := []saigetypes.Delta{
		saigetypes.PartStart{Kind: saigetypes.KindText},
		saigetypes.PartEnd{Part: saigetypes.Text("hi")},
		saigetypes.PartStart{Kind: saigetypes.KindThinking},
		saigetypes.PartDelta{Signature: "sig"},
		saigetypes.PartDelta{Args: `{"path":`},
		saigetypes.PartEnd{Part: saigetypes.ToolCallPart{ID: "call_1", Name: "grep", Arguments: map[string]any{"path": "."}}},
		saigetypes.UsageDelta{PromptTokens: 10, CompletionTokens: 3, Latency: time.Second},
		saigetypes.HandoffDelta{From: "a", To: "b"},
		saigetypes.FeedbackDelta{TargetNodeID: "n1"},
		saigetypes.DoneDelta{},
		saigetypes.ErrorDelta{},
		saigetypes.CitationDelta{Citation: saigetypes.Citation{Ordinal: 1}}, // no source and no quote: nothing to show
		saigetypes.PartDelta{}, // empty fragment: nothing to render
	}
	tr := native.NewTranslator()
	for _, d := range dropped {
		if c, ok := tr.Delta(d); ok {
			t.Errorf("%T became a %s event, want no event", d, c.Type())
		}
	}
}

// parentOf reads the Parent a body carries, as a subscriber that has only its
// JSON would.
func parentOf(t *testing.T, b event.Body) []string {
	t.Helper()
	var v struct {
		Parent []string `json:"parent"`
	}
	if err := json.Unmarshal(raw(b), &v); err != nil {
		t.Fatalf("decode %s: %v", b.Type(), err)
	}
	return v.Parent
}

// A nested agent's work is published under the calls it ran under, at every
// depth, so that its text never reads as part of the turn's answer. The turn's
// own agent's bodies carry no parent at all.
func TestNestedDeltasCarryTheCallPathTheyCameThrough(t *testing.T) {
	t.Parallel()

	deltas := []saigetypes.Delta{
		saigetypes.PartDelta{Text: "words"},
		saigetypes.PartDelta{Thinking: "thoughts"},
		saigetypes.PartStart{Kind: saigetypes.KindToolCall, ID: "c9", Name: "grep"},
		saigetypes.ToolExecStartDelta{ToolCallID: "c9", Name: "grep"},
		saigetypes.ToolExecEndDelta{ToolCallID: "c9", Name: "grep", Result: "ok"},
		saigetypes.CitationDelta{ToolCallID: "c9", Citation: saigetypes.Citation{Ordinal: 1, URI: "https://example.test/x"}},
		saigetypes.MarkerDelta{ToolCallID: "c9", ToolName: "grep"},
	}
	for _, path := range [][]string{nil, {"a"}, {"a", "b"}} {
		tr := native.NewTranslator()
		for _, d := range deltas {
			in := d
			for i := len(path) - 1; i >= 0; i-- {
				in = saigetypes.ToolExecDelta{ToolCallID: path[i], Inner: in}
			}
			b, ok := tr.Delta(in)
			if !ok {
				t.Fatalf("%T at %v produced no event", d, path)
			}
			if got := parentOf(t, b); !slices.Equal(got, path) {
				t.Errorf("%s at %v has parent %v", b.Type(), path, got)
			}
			if path == nil && strings.Contains(string(raw(b)), `"parent"`) {
				t.Errorf("the turn's own %s carries a parent: %s", b.Type(), raw(b))
			}
		}
	}
}

// Call ids repeat across agents. Two levels down, a call with its parent's
// and grandparent's id still gets its own arguments, and says where it ran.
func TestArgumentsAreKeptPerAgentTwoLevelsDown(t *testing.T) {
	t.Parallel()

	tr := native.NewTranslator()
	under := func(path []string, d saigetypes.Delta) saigetypes.Delta {
		for i := len(path) - 1; i >= 0; i-- {
			d = saigetypes.ToolExecDelta{ToolCallID: path[i], Inner: d}
		}
		return d
	}
	paths := [][]string{nil, {"c1"}, {"c1", "c1"}}
	for _, path := range paths {
		who := strings.Repeat("child ", len(path)) + "asked"
		tr.Delta(under(path, saigetypes.PartStart{Index: 0, Kind: saigetypes.KindToolCall, ID: "c1", Name: "grep"}))
		tr.Delta(under(path, saigetypes.PartEnd{Index: 0, Part: saigetypes.ToolCallPart{ID: "c1", Name: "grep", Arguments: map[string]any{"who": who}}}))
		tr.Ran("c1", "grep", map[string]any{"who": strings.Repeat("child ", len(path)) + "ran"}, path...)
	}
	// Finish them innermost first, as a delegation does.
	for i := len(paths) - 1; i >= 0; i-- {
		path := paths[i]
		c, _ := tr.Delta(under(path, saigetypes.ToolExecStartDelta{ToolCallID: "c1", Name: "grep"}))
		call := decodeCall(t, c)
		if want := `{"who":"` + strings.Repeat("child ", len(path)) + `asked"}`; string(call.Arguments) != want {
			t.Errorf("executing call at %v carries %s, want %s", path, call.Arguments, want)
		}
		if !slices.Equal(call.Parent, path) {
			t.Errorf("executing call at %v has parent %v", path, call.Parent)
		}
		c, _ = tr.Delta(under(path, saigetypes.ToolExecEndDelta{ToolCallID: "c1", Name: "grep"}))
		done := decodeDone(t, c)
		if want := `{"who":"` + strings.Repeat("child ", len(path)) + `ran"}`; string(done.Arguments) != want {
			t.Errorf("done at %v carries %s, want %s", path, done.Arguments, want)
		}
		if !slices.Equal(done.Parent, path) {
			t.Errorf("done at %v has parent %v", path, done.Parent)
		}
	}
}

// A footnote list has one entry per marker. A citation that repeats one the
// same agent already published, under the same marker, is dropped; one that
// gives the source another marker, or comes from another agent, is not.
func TestACitationIsPublishedOncePerAgentMarkerAndSource(t *testing.T) {
	t.Parallel()

	cite := func(call string, ordinal int, uri string) saigetypes.CitationDelta {
		return saigetypes.CitationDelta{ToolCallID: call, Citation: saigetypes.Citation{Ordinal: ordinal, URI: uri, Title: "Runbook"}}
	}
	tests := []struct {
		name string
		in   saigetypes.Delta
		want bool
	}{
		{"first citation of the source", cite("c1", 1, "https://example.test/runbook"), true},
		{"another call, same marker and source", cite("c2", 1, "https://EXAMPLE.test/runbook"), false},
		{"same source under another marker", cite("c2", 3, "https://example.test/runbook"), true},
		{"same marker for another source", cite("c2", 1, "https://example.test/other"), true},
		{"a nested agent's own numbering", saigetypes.ToolExecDelta{ToolCallID: "c1", Inner: cite("c1", 1, "https://example.test/runbook")}, true},
		{"the nested agent repeating itself", saigetypes.ToolExecDelta{ToolCallID: "c1", Inner: cite("c4", 1, "https://example.test/runbook")}, false},
	}
	tr := native.NewTranslator()
	for _, tc := range tests {
		b, ok := tr.Delta(tc.in)
		if ok != tc.want {
			t.Errorf("%s: published %v, want %v", tc.name, ok, tc.want)
		}
		if ok && b.Type() != event.TypeCitation {
			t.Errorf("%s: became %s", tc.name, b.Type())
		}
	}
}

// saige forwards a nested agent's marker without wrapping it, naming the call
// by the ids below the turn's agent joined with "/". It is published under
// those calls, like the rest of that agent's work, and names the call by the
// id its tool call events use.
func TestANestedAgentsMarkerIsPlacedUnderItsCalls(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		in     saigetypes.Delta
		id     string
		parent []string
	}{
		{"the turn's own call", saigetypes.MarkerDelta{ToolCallID: "c1", ToolName: "deploy"}, "c1", nil},
		{"forwarded from two levels down", saigetypes.MarkerDelta{ToolCallID: "a/b/c1", ToolName: "deploy"}, "c1", []string{"a", "b"}},
		{"forwarded inside a wrapped stream", saigetypes.ToolExecDelta{ToolCallID: "a", Inner: saigetypes.MarkerDelta{ToolCallID: "b/c1", ToolName: "deploy"}}, "c1", []string{"a", "b"}},
	}
	for _, tc := range tests {
		b, ok := native.NewTranslator().Delta(tc.in)
		if !ok {
			t.Fatalf("%s: no event", tc.name)
		}
		var m event.ToolMarker
		if err := json.Unmarshal(raw(b), &m); err != nil {
			t.Fatalf("%s: decode: %v", tc.name, err)
		}
		if m.ID != tc.id || !slices.Equal(m.Parent, tc.parent) || m.Name != "deploy" {
			t.Errorf("%s: got %s, want id %s under %v", tc.name, raw(b), tc.id, tc.parent)
		}
	}
}
