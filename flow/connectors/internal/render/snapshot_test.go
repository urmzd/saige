package render_test

import (
	"encoding/json"
	"testing"

	"github.com/urmzd/saige/flow/connectors/internal/render"
	"github.com/urmzd/saige/flow/event"
)

func TestSnapshotsPreserveReorderingCompactionAndEveryRenderedFamily(t *testing.T) {
	events := []event.Envelope{
		ev(1, event.TypeAccepted, ""), text(3, "later"), text(2, "earlier "),
		ev(6, event.TypeToolDone, `{"id":"tool","result":"done"}`),
		ev(4, event.TypeToolCall, `{"id":"tool","name":"read","arguments":{"x":1}}`),
		ev(5, event.TypeThinking, `{"text":"thinking"}`),
		ev(8, event.TypeCitation, `{"source":{"document_title":"second","source_uri":"https://example.com/second"}}`),
		ev(7, event.TypeCitation, `{"source":{"document_title":"first","source_uri":"https://example.com/first"}}`),
		ev(9, event.TypeAction, `{"summary":"changed","address":"github://a/b/issues/1"}`),
		ev(10, event.TypeParked, `{"question":{"id":"q","kind":"approval","prompt":"Continue?"}}`),
		ev(11, event.TypeResumed, `{"answer":{"question_id":"q","decision":"approve","by":"person"}}`),
		text(13, " tail"), text(13, " tail"), text(12, " middle"),
		ev(14, event.TypeStopped, `{"text":"final answer","reason":"requested"}`),
		ev(10, event.TypeParked, `{"question":{"id":"q","kind":"approval","prompt":"Continue?"}}`),
	}
	baseline, restored := render.New(), render.New()
	baseline.Window = 3
	restored.Window = 3
	for _, e := range events {
		wantChanged := baseline.Apply(e)
		gotChanged := restored.Apply(e)
		if gotChanged != wantChanged || !equalViews(baseline.View(), restored.View()) {
			t.Fatalf("continuation diverged at %d", e.Seq)
		}
		b, err := json.Marshal(restored)
		if err != nil {
			t.Fatal(err)
		}
		restored = render.New()
		if err := json.Unmarshal(b, restored); err != nil {
			t.Fatal(err)
		}
		if !equalViews(baseline.View(), restored.View()) {
			t.Fatalf("snapshot lost state at %d", e.Seq)
		}
	}
}

func TestUnknownSnapshotFormatFailsClosed(t *testing.T) {
	if err := json.Unmarshal([]byte(`{"Format":2}`), render.New()); err == nil {
		t.Fatal("accepted unknown snapshot format")
	}
}
