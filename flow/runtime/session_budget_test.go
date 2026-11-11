package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/urmzd/saige/durable/ledger"
	"github.com/urmzd/saige/durable/run"
	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/session"
)

func TestLateBudgetSettlementSurvivesRuntimeRestartWithoutChangingCompletedOutput(t *testing.T) {
	ctx := context.Background()
	s := session.NewMemory()
	l := ledger.NewMemory()
	rootRec, _ := s.Open(ctx, "t", session.Open{Parent: "console://t/root", Agent: "root"})
	childRec, _ := s.Open(ctx, "t", session.Open{Parent: rootRec.ID, Agent: "child"})
	grandRec, _ := s.Open(ctx, "t", session.Open{Parent: childRec.ID, Agent: "grand"})
	trigger := func(id string) event.Envelope {
		return event.Envelope{Tenant: "t", ID: id, Source: "console://t/root", Channel: "console://t/root", Type: event.TypeMessagePosted, Time: time.Now(), Data: event.MessagePosted{Request: event.Request{Text: "test"}}}
	}
	root := Input{Trigger: trigger("root"), Subscriber: "root", Session: &rootRec, Budget: &session.Budget{Requests: 20}}
	child := Input{Trigger: trigger("child"), Subscriber: "child", Session: &childRec, Parent: &ParentRef{RunID: root.runID()}, Budget: &session.Budget{Requests: 8}}
	grand := Input{Trigger: trigger("grand"), Subscriber: "grand", Session: &grandRec, Parent: &ParentRef{RunID: child.runID()}, Budget: &session.Budget{Requests: 3}}
	for _, in := range []Input{root, child, grand} {
		raw, _ := json.Marshal(in)
		if err := l.Accept(ctx, run.Run{ID: in.runID(), Name: "turn", Input: raw}); err != nil {
			t.Fatal(err)
		}
		if err := s.OpenBudget(ctx, "t", in.runID(), *in.Budget); err != nil {
			t.Fatal(err)
		}
	}
	reserve := func(account, id string, b session.Budget) {
		if _, err := s.ReserveBudget(ctx, "t", account, id, b); err != nil {
			t.Fatal(err)
		}
	}
	reserve(root.runID(), "child:"+child.runID(), *child.Budget)
	reserve(child.runID(), "child:"+grand.runID(), *grand.Budget)
	for _, in := range []Input{root, child, grand} {
		reserve(in.runID(), "attempt:1", session.Budget{Requests: 1})
		if err := s.SettleBudget(ctx, "t", in.runID(), "attempt:1", session.Budget{Requests: 1}); err != nil {
			t.Fatal(err)
		}
	}
	rt := &Runtime{sessions: s, stops: l}
	for _, in := range []Input{root, child} {
		if err := rt.closeRunBudget(ctx, in); err != nil {
			t.Fatal(err)
		}
		if err := l.Complete(ctx, in.runID(), 1, run.Result{Status: run.StatusSucceeded, Output: []byte(`{"text":"immutable"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := s.BudgetAccount(ctx, "t", root.runID())
	if before.Ready() {
		t.Fatal("late child reservation released before receipt")
	}
	// A new runtime has no local memory of how this tree was admitted.
	rt = &Runtime{sessions: s, stops: l}
	for range 2 {
		if err := rt.closeRunBudget(ctx, grand); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := s.BudgetAccount(ctx, "t", root.runID())
	if !got.Ready() || got.Spent().Requests != 3 {
		t.Fatalf("late settlement: %+v", got)
	}
	rec, _, err := l.Load(ctx, root.runID())
	if err != nil || string(rec.Output) != `{"text":"immutable"}` {
		t.Fatal("completed result rewritten", rec, err)
	}
	if grand.Session.ID != grandRec.ID || grand.Session.Key.Agent != "grand" {
		t.Fatal("ancestor walk mutated child's pinned identity")
	}
}

func TestBudgetChargesAbandonedAttemptConservatively(t *testing.T) {
	ctx := context.Background()
	s := session.NewMemory()
	b := session.Budget{Requests: 4}
	in := Input{Trigger: event.Envelope{Tenant: "t", ID: "crashed"}, Budget: &b}
	if err := s.OpenBudget(ctx, "t", in.runID(), b); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveBudget(ctx, "t", in.runID(), "attempt:1", b); err != nil {
		t.Fatal(err)
	}
	rt := &Runtime{sessions: s}
	if err := rt.closeRunBudget(ctx, in); err != nil {
		t.Fatal(err)
	}
	got, _ := s.BudgetAccount(ctx, "t", in.runID())
	if !got.Ready() || got.Spent() != b {
		t.Fatal(got)
	}
}

func TestAbandonedAdmissionIsFencedWithoutReleasingAcceptedChildren(t *testing.T) {
	ctx := context.Background()
	s := session.NewMemory()
	l := ledger.NewMemory()
	trigger := event.Envelope{Tenant: "t", ID: "parent", Source: "console://t/root", Channel: "console://t/root", Type: event.TypeMessagePosted, Time: time.Now(), Data: event.MessagePosted{Request: event.Request{Text: "test"}}}
	owner := Input{Trigger: trigger}
	raw, _ := json.Marshal(owner)
	if err := l.Accept(ctx, run.Run{ID: owner.runID(), Input: raw, Status: run.StatusPending}); err != nil {
		t.Fatal(err)
	}
	rt := &Runtime{sessions: s, stops: l, now: time.Now}
	claim := session.Reservation{Call: ownedAdmissionPrefix + "child-" + owner.runID() + "-question", Child: "child-session", Run: "predicted-child"}
	if ended, err := rt.reservationEnded(ctx, "t", claim); err != nil || ended {
		t.Fatal("released in-flight admission", ended, err)
	}
	if err := l.Complete(ctx, owner.runID(), 1, run.Result{Status: run.StatusSucceeded}); err != nil {
		t.Fatal(err)
	}
	if ended, err := rt.reservationEnded(ctx, "t", claim); err != nil || !ended {
		t.Fatal("abandoned admission not recovered", ended, err)
	}
	// A stale callback cannot replace the terminal fence and start new work.
	if err := l.Accept(ctx, run.Run{ID: claim.Run, Name: ActivityTurn, Status: run.StatusPending}); err != nil {
		t.Fatal(err)
	}
	child, _, err := l.Load(ctx, claim.Run)
	if err != nil || !child.Status.Terminal() {
		t.Fatal("late child escaped fence", child, err)
	}
	claim.Run = "accepted-child"
	if err := l.Accept(ctx, run.Run{ID: claim.Run, Name: ActivityTurn, Status: run.StatusPending}); err != nil {
		t.Fatal(err)
	}
	if ended, err := rt.reservationEnded(ctx, "t", claim); err != nil || ended {
		t.Fatal("released accepted async child", ended, err)
	}
	claim.Run = ""
	if ended, err := rt.reservationEnded(ctx, "t", claim); err != nil || !ended {
		t.Fatal("unbound abandoned slot leaked", ended, err)
	}
}

func TestBudgetSettlementIncludesRootAtMaximumSessionDepth(t *testing.T) {
	ctx := context.Background()
	s := session.NewMemory()
	l := ledger.NewMemory()
	rt := &Runtime{sessions: s, stops: l}
	var chain []Input
	for depth := 0; depth <= 32; depth++ {
		id := fmt.Sprint(depth)
		in := Input{Trigger: event.Envelope{Tenant: "t", ID: id, Source: "console://t/root", Channel: "console://t/root", Type: event.TypeMessagePosted, Time: time.Now(), Data: event.MessagePosted{Request: event.Request{Text: "test"}}}, Budget: &session.Budget{Requests: 1}}
		if depth > 0 {
			in.Parent = &ParentRef{RunID: chain[depth-1].runID()}
		}
		raw, _ := json.Marshal(in)
		if err := l.Accept(ctx, run.Run{ID: in.runID(), Input: raw}); err != nil {
			t.Fatal(err)
		}
		if err := s.OpenBudget(ctx, "t", in.runID(), *in.Budget); err != nil {
			t.Fatal(err)
		}
		if depth > 0 {
			if _, err := s.ReserveBudget(ctx, "t", in.Parent.RunID, "child:"+in.runID(), *in.Budget); err != nil {
				t.Fatal(err)
			}
		}
		chain = append(chain, in)
	}
	for _, in := range chain {
		if err := rt.closeRunBudget(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	a, err := s.BudgetAccount(ctx, "t", chain[0].runID())
	if err != nil || !a.Ready() {
		t.Fatal("maximum-depth root remained unsettled", a, err)
	}
}
