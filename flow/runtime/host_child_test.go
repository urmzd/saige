package runtime_test

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/urmzd/saige/flow/runtime"
	"github.com/urmzd/saige/flow/session"
)

func TestHostChildAdmissionRecordsOnlyVerifiedLedgerAncestry(t *testing.T) {
	w := newWorld(t)
	rt := w.runtime(t, newExecutor("done"), runtime.WithSessions(session.NewMemory()))
	ctx := t.Context()
	parent, err := rt.AcceptOwn(ctx, tag("docs-bot", "parent", "parent").event(), "docs-bot")
	if err != nil {
		t.Fatal(err)
	}
	other, err := rt.AcceptOwn(ctx, tag("docs-bot", "other", "other").event(), "docs-bot")
	if err != nil {
		t.Fatal(err)
	}
	foreign := tag("docs-bot", "foreign", "foreign")
	foreign.Tenant = "globex"
	foreignParent, err := rt.AcceptOwn(ctx, foreign.event(), "docs-bot")
	if err != nil {
		t.Fatal(err)
	}
	child := tag("docs-bot", "child", "child").event()
	for _, bad := range []string{"", "missing-parent", runtime.RunID(child.Tenant, child.ID), foreignParent.RunID} {
		if _, err := rt.AcceptOwnChild(ctx, child, "docs-bot", bad); !errors.Is(err, runtime.ErrNotAChild) {
			t.Fatalf("parent %q accepted: %v", bad, err)
		}
	}
	accepted, err := rt.AcceptOwnChild(ctx, child, "docs-bot", parent.RunID)
	if err != nil {
		t.Fatal(err)
	}
	held, _, err := w.ledger.Load(ctx, accepted.RunID)
	if err != nil || held.ParentRunID != parent.RunID {
		t.Fatal("ledger lost lineage", held, err)
	}
	var in runtime.Input
	if err = json.Unmarshal(held.Input, &in); err != nil || in.Parent == nil || in.Parent.RunID != parent.RunID || in.Parent.QuestionID != "" || in.Session == nil {
		t.Fatal("host child became a parent wait or lost session", in, err)
	}
	if replay, err := rt.AcceptOwnChild(ctx, child, "docs-bot", parent.RunID); err != nil || replay.RunID != accepted.RunID {
		t.Fatal("idempotent replay changed", replay, err)
	}
	if _, err := rt.AcceptOwnChild(ctx, child, "docs-bot", other.RunID); !errors.Is(err, runtime.ErrNotAChild) {
		t.Fatal("reparented durable child", err)
	}
}

func TestHostChildAdmissionCannotReparentDuringConcurrentReplay(t *testing.T) {
	w := newWorld(t)
	rt := w.runtime(t, newExecutor("done"), runtime.WithSessions(session.NewMemory()))
	ctx := t.Context()
	parents := make([]string, 2)
	for i, id := range []string{"first", "second"} {
		acc, err := rt.AcceptOwn(ctx, tag("docs-bot", id, id).event(), "docs-bot")
		if err != nil {
			t.Fatal(err)
		}
		parents[i] = acc.RunID
	}
	child := tag("docs-bot", "raced-child", "child").event()
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(parent string) {
			defer wg.Done()
			acc, err := rt.AcceptOwnChild(ctx, child, "docs-bot", parent)
			if errors.Is(err, runtime.ErrNotAChild) {
				return
			}
			if err != nil {
				t.Errorf("admit: %v", err)
				return
			}
			held, _, err := w.ledger.Load(ctx, acc.RunID)
			if err != nil || held.ParentRunID != parent {
				t.Errorf("acknowledged another parent's child: %v %v", held.ParentRunID, err)
			}
		}(parents[i%len(parents)])
	}
	wg.Wait()
}
