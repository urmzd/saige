package render_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/flow/connectors/internal/render"
	"github.com/urmzd/saige/flow/event"
)

// ev builds a run event the way the bus delivers one, decoding data into the
// type's body.
func ev(seq uint64, typ, data string) event.Envelope {
	if data == "" {
		data = "{}"
	}
	body, err := event.Decode(typ, json.RawMessage(data))
	if err != nil {
		panic(err)
	}
	return event.Envelope{
		ID:      fmt.Sprintf("run_1:%d", seq),
		Source:  "fiber://run/run_1",
		Type:    typ,
		Channel: "slack://T01/C02",
		Tenant:  "acme",
		Run:     "run_1",
		Seq:     seq,
		Agent:   "docs-bot",
		Version: "3",
		Time:    time.Unix(1700000000, 0).UTC().Add(time.Duration(seq) * time.Second),
		Data:    body,
	}
}

func text(seq uint64, s string) event.Envelope {
	return ev(seq, event.TypeText, `{"text":`+quote(s)+`}`)
}

func quote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestApplyingTheSameEventTwiceLeavesTheDocumentIdentical(t *testing.T) {
	events := []event.Envelope{
		ev(1, event.TypeAccepted, ""),
		text(2, "hello "),
		ev(3, event.TypeCitation, `{"source":{"document_title":"Design","source_uri":"https://example.com/d"}}`),
		text(4, "world"),
		ev(5, event.TypeCompleted, ""),
	}

	once := render.New()
	for _, e := range events {
		once.Apply(e)
	}

	twice := render.New()
	for _, e := range events {
		twice.Apply(e)
		if changed := twice.Apply(e); changed {
			t.Errorf("second Apply of seq %d reported a change", e.Seq)
		}
	}

	if got, want := twice.View(), once.View(); !equalViews(got, want) {
		t.Errorf("redelivery changed the document:\n got %+v\nwant %+v", got, want)
	}
}

func TestTextIsAssembledInSequenceOrderNotArrivalOrder(t *testing.T) {
	tests := []struct {
		name  string
		order []uint64
	}{
		{name: "in order", order: []uint64{1, 2, 3, 4}},
		{name: "reversed", order: []uint64{4, 3, 2, 1}},
		{name: "one late arrival", order: []uint64{1, 3, 2, 4}},
		{name: "last first", order: []uint64{4, 1, 2, 3}},
		{name: "with redelivery", order: []uint64{1, 2, 2, 4, 3, 1, 4}},
	}
	fragments := map[uint64]string{1: "the ", 2: "quick ", 3: "brown ", 4: "fox"}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := render.New()
			for _, seq := range tc.order {
				d.Apply(text(seq, fragments[seq]))
			}
			if got := d.View().Text; got != "the quick brown fox" {
				t.Errorf("Text = %q, want %q", got, "the quick brown fox")
			}
		})
	}
}

func TestALateFragmentOutsideTheWindowIsDroppedRatherThanAppendedOutOfPlace(t *testing.T) {
	d := &render.Doc{Window: 4}
	// Seq 1 never arrives, so the mark cannot advance contiguously; the window
	// forces the oldest fragments to settle.
	for seq := uint64(2); seq <= 10; seq++ {
		d.Apply(text(seq, "x"))
	}
	if got := d.View().Text; got != strings.Repeat("x", 9) {
		t.Fatalf("Text = %q, want nine fragments", got)
	}
	if changed := d.Apply(text(1, "STALE")); changed {
		t.Error("a fragment older than the window was applied")
	}
	if got := d.View().Text; strings.Contains(got, "STALE") {
		t.Errorf("Text = %q, must not contain the stale fragment", got)
	}
}

func TestStatusFollowsTheHighestSequenceNumberNotTheLastArrival(t *testing.T) {
	tests := []struct {
		name  string
		order []event.Envelope
		want  render.Status
	}{
		{
			name:  "in order",
			order: []event.Envelope{ev(1, event.TypeAccepted, ""), ev(2, event.TypeStarted, ""), ev(9, event.TypeCompleted, "")},
			want:  render.StatusDone,
		},
		{
			name:  "completion arrives before the start it followed",
			order: []event.Envelope{ev(9, event.TypeCompleted, ""), ev(2, event.TypeStarted, "")},
			want:  render.StatusDone,
		},
		{
			name:  "no lifecycle event at all",
			order: []event.Envelope{text(4, "hi")},
			want:  render.StatusUnknown,
		},
		{
			name:  "failure carries its message",
			order: []event.Envelope{ev(1, event.TypeStarted, ""), ev(2, event.TypeFailed, `{"error":"provider timeout"}`)},
			want:  render.StatusFailed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := render.New()
			for _, e := range tc.order {
				d.Apply(e)
			}
			v := d.View()
			if v.Status != tc.want {
				t.Errorf("Status = %q, want %q", v.Status, tc.want)
			}
			if tc.want == render.StatusFailed && v.Error != "provider timeout" {
				t.Errorf("Error = %q, want the failure message", v.Error)
			}
			if v.Terminal() != tc.want.Terminal() {
				t.Errorf("Terminal = %v, want %v", v.Terminal(), tc.want.Terminal())
			}
		})
	}
}

func TestCitationsAreNumberedBySequenceAndDeduplicated(t *testing.T) {
	d := render.New()
	d.Apply(ev(7, event.TypeCitation, `{"source":{"document_title":"Second","source_uri":"https://example.com/2"}}`))
	d.Apply(ev(3, event.TypeCitation, `{"source":{"document_title":"First","source_uri":"https://example.com/1"}}`))
	// The same source cited again is one footnote, not two.
	d.Apply(ev(9, event.TypeCitation, `{"source":{"document_title":"First","source_uri":"https://example.com/1"}}`))

	cites := d.View().Citations
	if len(cites) != 2 {
		t.Fatalf("got %d citations, want 2: %+v", len(cites), cites)
	}
	if cites[0].Title != "First" || cites[0].N != 1 || cites[0].Ref() != "1" {
		t.Errorf("first citation = %+v, want First numbered 1", cites[0])
	}
	if cites[1].Title != "Second" || cites[1].N != 2 {
		t.Errorf("second citation = %+v, want Second numbered 2", cites[1])
	}
}

func TestCitationsPreferAnAgentSuppliedLabelAndFallBackToTheSection(t *testing.T) {
	d := render.New()
	d.Apply(ev(2, event.TypeCitation, `{"label":"rfc","source":{"document_title":"RFC 9110","source_uri":"https://example.com/rfc"}}`))
	d.Apply(ev(3, event.TypeCitation, `{"text":"a snippet","source":{"section_heading":"Design doc","source_uri":"https://example.com/d"}}`))

	cites := d.View().Citations
	if len(cites) != 2 {
		t.Fatalf("got %d citations, want 2", len(cites))
	}
	if cites[0].Ref() != "rfc" || cites[0].URI != "https://example.com/rfc" {
		t.Errorf("labelled citation = %+v", cites[0])
	}
	if cites[1].Title != "Design doc" || cites[1].URI != "https://example.com/d" || cites[1].Snippet != "a snippet" {
		t.Errorf("citation from a section = %+v", cites[1])
	}
}

func TestToolCallAndCompletionJoinOnTheirIDInEitherOrder(t *testing.T) {
	tests := []struct {
		name  string
		order []event.Envelope
	}{
		{name: "call then done", order: []event.Envelope{
			ev(1, event.TypeToolCall, `{"id":"t1","name":"search","arguments":{"q":  "fiber"}}`),
			ev(2, event.TypeToolDone, `{"id":"t1","result":"3 hits"}`),
		}},
		{name: "done then call", order: []event.Envelope{
			ev(2, event.TypeToolDone, `{"id":"t1","result":"3 hits"}`),
			ev(1, event.TypeToolCall, `{"id":"t1","name":"search","arguments":{"q":  "fiber"}}`),
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := render.New()
			for _, e := range tc.order {
				d.Apply(e)
			}
			tools := d.View().Tools
			if len(tools) != 1 {
				t.Fatalf("got %d tools, want 1: %+v", len(tools), tools)
			}
			got := tools[0]
			want := render.Tool{ID: "t1", Name: "search", Args: `{"q":"fiber"}`, Result: "3 hits", Done: true}
			if got.ID != want.ID || got.Name != want.Name || got.Args != want.Args ||
				got.Result != want.Result || got.Error != want.Error || got.Done != want.Done {
				t.Errorf("tool = %+v, want %+v", got, want)
			}
		})
	}
}

func TestTextKeepsItsWhitespaceAndAnEmptyBodyAddsNothing(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "text", data: `{"text":"hi"}`, want: "hi"},
		{name: "whitespace is kept", data: `{"text":" hi "}`, want: " hi "},
		{name: "empty body contributes nothing", data: ``, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := render.New()
			d.Apply(ev(1, event.TypeText, tc.data))
			if got := d.View().Text; got != tc.want {
				t.Errorf("Text = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestActionsRenderAValidAddressAndDropABrokenOne(t *testing.T) {
	tests := []struct {
		name string
		data string
		want render.Action
	}{
		{
			name: "address as a uri",
			data: `{"name":"comment","summary":"commented","address":"github://urmzd/fiber/issues/42"}`,
			want: render.Action{Summary: "commented", Address: "github://urmzd/fiber/issues/42"},
		},
		{
			name: "broken address is dropped, not rendered as a broken link",
			data: `{"name":"note","summary":"noted","address":"not an address"}`,
			want: render.Action{Summary: "noted"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := render.New()
			d.Apply(ev(1, event.TypeAction, tc.data))
			actions := d.View().Actions
			if len(actions) != 1 {
				t.Fatalf("got %d actions, want 1", len(actions))
			}
			if actions[0].Summary != tc.want.Summary || actions[0].Address != tc.want.Address {
				t.Errorf("action = %+v, want %+v", actions[0], tc.want)
			}
		})
	}
}

func TestAnUnknownKindReportsNoChangeSoTheSinkSkipsTheRoundTrip(t *testing.T) {
	d := render.New()
	if changed := d.Apply(ev(1, "run.delta.something.new.v1", `{"text":"hi"}`)); changed {
		t.Error("an unknown kind reported a change")
	}
	if v := d.View(); !v.Empty() {
		t.Errorf("an unknown kind contributed to the document: %+v", v)
	}
	// It is still recorded, so a redelivery is cheap and the mark still moves.
	if changed := d.Apply(text(2, "hi")); !changed {
		t.Error("a following text delta was swallowed")
	}
	if got := d.View().Text; got != "hi" {
		t.Errorf("Text = %q, want %q", got, "hi")
	}
}

func TestIdentityIsTakenFromTheFirstEventApplied(t *testing.T) {
	d := render.New()
	d.Apply(text(4, "hi"))
	v := d.View()
	if v.Agent != "docs-bot" || v.RunID != "run_1" || v.Version != "3" || v.Channel != "slack://T01/C02" || v.Tenant != "acme" {
		t.Errorf("identity = %+v, want the event's agent/run/version/channel/tenant", v)
	}
	if v.Seq != 4 {
		t.Errorf("Seq = %d, want 4", v.Seq)
	}
}

func TestThinkingAccumulatesSeparatelyFromTheAnswer(t *testing.T) {
	d := render.New()
	d.Apply(ev(1, event.TypeThinking, `{"text":"let me check "}`))
	d.Apply(text(2, "the answer is 4"))
	d.Apply(ev(3, event.TypeThinking, `{"text":"the docs"}`))

	v := d.View()
	if v.Text != "the answer is 4" {
		t.Errorf("Text = %q, want the answer only", v.Text)
	}
	if v.Thinking != "let me check the docs" {
		t.Errorf("Thinking = %q, want the reasoning only", v.Thinking)
	}
}

func TestMemoryStaysBoundedByTheWindowUnderASparseStream(t *testing.T) {
	// A route selecting only lifecycle kinds receives a sparse subsequence, so
	// the contiguous rule never fires and only the window bounds the document.
	d := &render.Doc{Window: 8}
	for i := uint64(1); i <= 500; i++ {
		d.Apply(text(i*10, "x"))
	}
	if got := len(d.View().Text); got != 500 {
		t.Fatalf("Text length = %d, want 500 fragments", got)
	}
	// Everything but the newest window has settled, so a redelivery of an old
	// sequence number is still refused.
	if changed := d.Apply(text(10, "x")); changed {
		t.Error("a settled sequence number was applied again")
	}
}

func equalViews(a, b render.View) bool {
	if a.Text != b.Text || a.Thinking != b.Thinking || a.Status != b.Status || a.Error != b.Error {
		return false
	}
	if a.Seq != b.Seq || !a.At.Equal(b.At) {
		return false
	}
	if len(a.Citations) != len(b.Citations) || len(a.Tools) != len(b.Tools) || len(a.Actions) != len(b.Actions) {
		return false
	}
	for i := range a.Citations {
		x, y := a.Citations[i], b.Citations[i]
		if x.N != y.N || x.Ref() != y.Ref() || x.Title != y.Title || x.URI != y.URI || x.Snippet != y.Snippet {
			return false
		}
	}
	for i := range a.Tools {
		x, y := a.Tools[i], b.Tools[i]
		if x.ID != y.ID || x.Name != y.Name || x.Args != y.Args || x.Result != y.Result || x.Error != y.Error || x.Done != y.Done {
			return false
		}
	}
	for i := range a.Actions {
		if a.Actions[i].Summary != b.Actions[i].Summary || a.Actions[i].Address != b.Actions[i].Address {
			return false
		}
	}
	return true
}

func TestFinalAnswerReplacesPartialAndLateDeltas(t *testing.T) {
	for _, events := range [][]event.Envelope{
		{ev(5, event.TypeCompleted, `{"text":"complete answer"}`)},
		{text(2, "partial"), ev(5, event.TypeCompleted, `{"text":"complete answer"}`), text(3, " late")},
		{ev(5, event.TypeCompleted, `{"text":"complete answer"}`), ev(4, event.TypeCompleted, `{"text":"stale answer"}`)},
	} {
		var doc render.Doc
		for _, e := range events {
			doc.Apply(e)
		}
		if got := doc.View().Text; got != "complete answer" {
			t.Fatalf("final answer = %q", got)
		}
	}
}

// A stopped run is terminal and is its own status: not done, because the text
// is an unfinished answer, and not failed, because nothing went wrong. The
// partial answer and the reason travel on the event, so a route that selected
// only lifecycle kinds still shows both.
func TestAStoppedRunIsTerminalAndCarriesItsPartialAnswerAndReason(t *testing.T) {
	d := render.New()
	d.Apply(ev(1, event.TypeStarted, `{}`))
	d.Apply(text(2, "Half an "))
	d.Apply(text(3, "answer"))
	if !d.Apply(ev(9, event.TypeStopped, `{"text":"Half an answer","reason":"Stopped by a user","by":"slack:U1"}`)) {
		t.Fatal("applying lifecycle.stopped reported no change")
	}

	v := d.View()
	if v.Status != render.StatusStopped || !v.Terminal() {
		t.Fatalf("status = %q, terminal = %v; want stopped and terminal", v.Status, v.Terminal())
	}
	if v.Status == render.StatusDone || v.Status == render.StatusFailed || v.Error != "" {
		t.Fatalf("a stopped run renders as %q with error %q", v.Status, v.Error)
	}
	if v.Text != "Half an answer" || v.Reason != "Stopped by a user" {
		t.Errorf("text = %q, reason = %q", v.Text, v.Reason)
	}

	// At-least-once: the same event again changes nothing.
	if d.Apply(ev(9, event.TypeStopped, `{"text":"Half an answer","reason":"Stopped by a user"}`)) {
		t.Error("a redelivered lifecycle.stopped changed the document")
	}

	// A sink that was sent the terminal event alone still has the answer.
	only := render.New()
	only.Apply(ev(9, event.TypeStopped, `{"text":"Half an answer"}`))
	if got := only.View(); got.Text != "Half an answer" || got.Status != render.StatusStopped || got.Reason != "" {
		t.Errorf("lifecycle-only view = %+v", got)
	}
}

// Token counts and cost are accounting for whoever pays, not something a
// Slack channel or an issue thread should show, so usage events change
// nothing on a chat surface and cost the sink no edit. The usage riding on the
// completed event does not reach the document either.
func TestUsageIsNeverRenderedOnAChatSurface(t *testing.T) {
	d := render.New()
	if d.Apply(ev(1, event.TypeUsageCall, `{"accounting_id":"r/a1/1","prompt_tokens":12000,"cost":{"micros":40500,"currency":"USD"}}`)) {
		t.Error("a usage.call event reported a change")
	}
	if d.Apply(ev(2, event.TypeUsageChild, `{"accounting_id":"run:r9","usage":{"prompt_tokens":5}}`)) {
		t.Error("a usage.child event reported a change")
	}
	if v := d.View(); !v.Empty() {
		t.Errorf("usage contributed to the document: %+v", v)
	}
	d.Apply(ev(3, event.TypeCompleted, `{"text":"Done.","usage":{"total":{"prompt_tokens":12000,"cost":{"micros":40500,"currency":"USD"}}}}`))
	if v := d.View(); strings.Contains(fmt.Sprintf("%+v", v), "40500") || strings.Contains(fmt.Sprintf("%+v", v), "12000") {
		t.Errorf("the run's usage reached the document: %+v", v)
	}
}
