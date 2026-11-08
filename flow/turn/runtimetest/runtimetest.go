// Package runtimetest is the conformance suite every turn.Runtime passes.
//
//	runtimetest.Run(t, runtimetest.Harness{New: ..., Pinned: ..., Input: ...})
package runtimetest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/turn"
)

// Harness is what the suite runs against.
type Harness struct {
	// New returns the Runtime under test. It is called once per check, and
	// each Runtime it returns must answer Input with some text.
	New func(t *testing.T) turn.Runtime
	// Pinned is the definition every check runs.
	Pinned turn.Pinned
	// Input is what every check runs. Its trigger must be answered with
	// some text.
	Input turn.Input
}

// Run runs every check.
func Run(t *testing.T, h Harness) {
	t.Helper()
	if h.New == nil {
		t.Fatal("runtimetest: the harness has no New")
	}
	t.Run("EmitsOnlyRegisteredBodies", func(t *testing.T) { emitsOnlyRegisteredBodies(t, h) })
	t.Run("TextIsTheConcatenatedTextDeltas", func(t *testing.T) { textIsTheDeltas(t, h) })
	t.Run("StopOnContextYieldsStopped", func(t *testing.T) { stopYieldsStopped(t, h) })
	t.Run("EmitErrorAbortsTheTurn", func(t *testing.T) { emitErrorAborts(t, h) })
}

func collect(ctx context.Context, t *testing.T, h Harness) ([]event.Body, turn.Result) {
	t.Helper()
	var got []event.Body
	res, err := h.New(t).Turn(ctx, h.Pinned, h.Input, func(_ context.Context, b event.Body) error {
		got = append(got, b)
		return nil
	})
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	return got, res
}

func emitsOnlyRegisteredBodies(t *testing.T, h Harness) {
	got, _ := collect(t.Context(), t, h)
	if len(got) == 0 {
		t.Fatal("the turn emitted nothing; the harness must answer with some text")
	}
	for i, b := range got {
		if b == nil {
			t.Fatalf("body %d is nil", i)
		}
		if _, unknown := b.(event.Unknown); unknown {
			t.Fatalf("body %d is Unknown(%q): a runtime emits only registered types", i, b.Type())
		}
		if _, ok := event.Lookup(b.Type()); !ok {
			t.Fatalf("body %d is of type %q, which the catalog does not register", i, b.Type())
		}
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatalf("body %d (%s) does not marshal: %v", i, b.Type(), err)
		}
		back, err := event.Decode(b.Type(), raw)
		if err != nil {
			t.Fatalf("body %d (%s) does not decode: %v", i, b.Type(), err)
		}
		if back.Type() != b.Type() {
			t.Fatalf("body %d decodes as %q, want %q", i, back.Type(), b.Type())
		}
	}
}

func textIsTheDeltas(t *testing.T, h Harness) {
	got, res := collect(t.Context(), t, h)
	var text strings.Builder
	for _, b := range got {
		if d, ok := b.(event.Text); ok {
			text.WriteString(d.Text)
		}
	}
	if res.Text == "" {
		t.Fatal("the turn answered nothing; the harness must answer with some text")
	}
	if res.Text != text.String() {
		t.Fatalf("Result.Text = %q, the %s bodies say %q", res.Text, event.TypeText, text.String())
	}
}

func stopYieldsStopped(t *testing.T, h Harness) {
	stop := make(chan turn.Stop, 1)
	stop <- turn.Stop{Reason: "that is enough", By: "runtimetest"}
	ctx := turn.WithStopSignal(t.Context(), stop)
	_, res := collect(ctx, t, h)
	if res.Stopped == nil {
		t.Fatal("a turn asked to stop on its context did not report Result.Stopped")
	}
	if res.Stopped.Reason != "that is enough" || res.Stopped.By != "runtimetest" {
		t.Fatalf("Result.Stopped = %+v, want the request that was sent", *res.Stopped)
	}
}

func emitErrorAborts(t *testing.T, h Harness) {
	refused := errors.New("runtimetest: emit refused")
	_, err := h.New(t).Turn(t.Context(), h.Pinned, h.Input, func(context.Context, event.Body) error {
		return refused
	})
	if err == nil {
		t.Fatal("a turn whose emit failed returned no error")
	}
}
