package runtime

import (
	"context"
	"errors"
	"github.com/urmzd/saige/flow/event"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/flow/session"
)

func TestSessionReadRequiresTreeMembershipOrExplicitGrant(t *testing.T) {
	ctx := context.Background()
	store := session.NewMemory()
	rt := &Runtime{sessions: store}
	open := func(parent, agent string) session.Record {
		r, err := store.Open(ctx, "tenant", session.Open{Parent: parent, Agent: agent})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	a := open("console://tenant/a", "lead")
	child := open(a.ID, "helper")
	other := open("console://tenant/b", "reviewer")
	if _, err := rt.sessionAccess(ctx, child, a.ID); err != nil {
		t.Fatal("ancestor read denied", err)
	}
	if _, err := rt.sessionAccess(ctx, a, child.ID); err != nil {
		t.Fatal("child read denied", err)
	}
	if _, err := rt.sessionAccess(ctx, a, other.ID); !errors.Is(err, session.ErrDenied) {
		t.Fatal("unshared tree read", err)
	}
	if err := store.Link(ctx, "tenant", a.ID, session.Link{Target: other.ID, Kind: "context"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.sessionAccess(ctx, a, other.ID); !errors.Is(err, session.ErrDenied) {
		t.Fatal("link granted read access", err)
	}
	if err := store.GrantRead(ctx, "tenant", other.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.sessionAccess(ctx, a, other.ID); err != nil {
		t.Fatal("explicit grant ignored", err)
	}
	if _, err := rt.sessionAccess(ctx, child, other.ID); !errors.Is(err, session.ErrDenied) {
		t.Fatal("grant widened to another agent", err)
	}
	if err := store.RevokeRead(ctx, "tenant", other.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.sessionAccess(ctx, a, other.ID); !errors.Is(err, session.ErrDenied) {
		t.Fatal("revocation ignored", err)
	}
}

func TestRevokingAReadGrantWhileParkedHidesEveryListenResult(t *testing.T) {
	ctx := context.Background()
	store := session.NewMemory()
	reader, _ := store.Open(ctx, "t", session.Open{Parent: "console://t/reader", Agent: "reader"})
	target, _ := store.Open(ctx, "t", session.Open{Parent: "console://t/private", Agent: "private"})
	if err := store.GrantRead(ctx, "t", target.ID, reader.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeRead(ctx, "t", target.ID, reader.ID); err != nil {
		t.Fatal(err)
	}
	rt := &Runtime{sessions: store, now: time.Now}
	for _, mode := range []string{"end", "message"} {
		answer, ready := rt.sessionListenEnded(ctx, Input{Session: &reader, Trigger: event.Envelope{Tenant: "t"}}, event.Question{ID: "q"}, []sessionWait{{Session: target.ID, Run: "secret-run", Until: mode}})
		if !ready || answer == nil || !strings.Contains(answer.Text, "session access denied") {
			t.Fatalf("%s revealed revoked target: %+v", mode, answer)
		}
	}
}
