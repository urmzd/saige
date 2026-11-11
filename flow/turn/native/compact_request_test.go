package native_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	saigetypes "github.com/urmzd/saige/agent/types"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/turn/native"
)

// askingTool is a lookup that, on the calls ask says, asks the turn to
// compact, the way a host pressing a button while the tool runs would.
//
// It sends each request twice on an unbuffered channel. The second send
// completes only once the turn has finished handling the first, so the tool
// returns with the request already pending, and the test does not race the
// goroutine that receives it. Two requests before one model call are one
// compaction, which is part of what the tests check.
type askingTool struct {
	compact chan<- native.CompactRequest
	ask     func(call int) bool

	mu    sync.Mutex
	calls int
}

func (t *askingTool) Definition() saigetypes.ToolDef {
	return saigetypes.ToolDef{Name: "lookup", Description: "Look something up", Parameters: saigetypes.ParameterSchema{Type: "object"}}
}

func (t *askingTool) Execute(ctx context.Context, _ map[string]any) (string, error) {
	t.mu.Lock()
	t.calls++
	call := t.calls
	t.mu.Unlock()
	if t.ask(call) {
		for range 2 {
			select {
			case t.compact <- native.CompactRequest{By: "ada"}:
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
	}
	return "row of results. row of results. row of results.", nil
}

// runAsking runs a tool loop whose tool asks for compaction on the calls ask
// says, and returns the result, the context.compacted bodies the turn
// published, and how many summaries the provider was asked for.
func runAsking(t *testing.T, c native.Compaction, turns int, ask func(int) bool) (native.Result, []event.Compaction, int) {
	t.Helper()
	p := &loopProvider{turns: turns}
	compact := make(chan native.CompactRequest)
	rev := offlineRevision(1, "You look things up.")
	rev.Spec.Tools = []string{"lookup"}
	r, err := native.New(rev,
		native.WithProvider(native.ProviderOffline, func(native.Spec) (saigetypes.Provider, error) { return p, nil }),
		native.WithTools(native.StaticTools(&askingTool{compact: compact, ask: ask})),
		native.WithMaxIter(turns+5),
		native.WithCompaction(c),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, res := collect(t, r, native.Turn{Text: "find the answer", Compact: compact})
	if res.Text != "done" {
		t.Fatalf("turn answered %q, want it to finish the loop", res.Text)
	}
	var published []event.Compaction
	for _, ch := range got {
		if ch.Type() != event.TypeCompacted {
			continue
		}
		var body event.Compaction
		if err := json.Unmarshal(raw(ch), &body); err != nil {
			t.Fatalf("decode %s: %v", ch.Type(), err)
		}
		published = append(published, body)
	}
	summaries := 0
	for _, req := range p.calls() {
		if isSummaryRequest(req) {
			summaries++
		}
	}
	return res, published, summaries
}

// A host's request compacts the turn before its next model call, once however
// many times it was pressed, and the turn says so on its stream: who asked,
// and that it was asked rather than forced by size.
func TestARequestedCompactionHappensOnceAndIsPublished(t *testing.T) {
	t.Parallel()

	res, published, summaries := runAsking(t,
		native.Compaction{OnRequest: true, KeepTurns: 1}, 5,
		func(call int) bool { return call == 3 })

	if summaries != 1 || len(res.Compactions) != 1 {
		t.Fatalf("turn wrote %d summaries and recorded %d compactions, want one of each", summaries, len(res.Compactions))
	}
	if len(published) != 1 {
		t.Fatalf("turn published %d %s events, want 1", len(published), event.TypeCompacted)
	}
	got := published[0]
	if got != res.Compactions[0] {
		t.Errorf("published %+v, but the result records %+v", got, res.Compactions[0])
	}
	if got.Trigger != native.TriggerRequested || got.By != "ada" {
		t.Errorf("compaction reports trigger %q by %q, want %q by %q", got.Trigger, got.By, native.TriggerRequested, "ada")
	}
	if got.Summarized == 0 {
		t.Errorf("compaction %+v summarized nothing", got)
	}
}

// Taking requests is something a deployment turns on. Without it a request
// changes nothing, and a deployment that takes requests and sets no budget
// never compacts unasked.
func TestCompactionHappensOnlyWhenConfiguredAndAsked(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		c    native.Compaction
		ask  func(int) bool
	}{
		{"asked, but requests are not taken", native.Compaction{KeepTurns: 1}, func(int) bool { return true }},
		{"requests are taken, nobody asks", native.Compaction{OnRequest: true, KeepTurns: 1}, func(int) bool { return false }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res, published, summaries := runAsking(t, tc.c, 6, tc.ask)
			if summaries != 0 || len(res.Compactions) != 0 || len(published) != 0 {
				t.Errorf("turn wrote %d summaries, recorded %d compactions and published %d; want none",
					summaries, len(res.Compactions), len(published))
			}
		})
	}
}

// The anti-thrash rule spaces out the compactions a budget makes. It does not
// apply to a request: a person asked for this one, and gets it.
func TestARequestIsNotHeldBackByTheBudgetsSpacing(t *testing.T) {
	t.Parallel()

	const keep = 2
	res, _, _ := runAsking(t,
		native.Compaction{OnRequest: true, MaxInputTokens: 1, KeepTurns: keep}, 10,
		func(call int) bool { return call >= 4 })

	requested := 0
	for _, c := range res.Compactions {
		if c.Trigger == native.TriggerRequested {
			requested++
		}
	}
	// A request is pending before each of the last seven model calls, and
	// each finds something old enough to summarize, so each compacts. Under
	// the spacing rule they would be keep calls apart: three or four.
	if requested < 6 {
		t.Errorf("only %d of %d compactions were the requested ones (%+v); requests were held back", requested, len(res.Compactions), res.Compactions)
	}
}
