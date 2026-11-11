package github

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connectors"
	"github.com/urmzd/saige/flow/deliverystate"
	"github.com/urmzd/saige/flow/event"
)

func TestReactionRemovalRequiresStableCompleteScansAndSurvivesRestart(t *testing.T) {
	api := NewFake()
	target := address.MustParse("github://acme/repo/pull/7?review_comment=10")
	id, _ := api.SeedReaction(target, "+1", "alice", "User")
	st := deliverystate.NewMemory()
	removed := []string{}
	fail := true
	cfg := Config{API: api, Tenant: "tenant", DeliveryState: st, DeliveryScope: "install", Feedback: connectors.FeedbackHandler{
		Resolve: func(context.Context, string, address.Address) (connectors.FeedbackTarget, error) {
			return connectors.FeedbackTarget{Found: true, Run: "r", Finding: "f"}, nil
		},
		Record: func(context.Context, string, string, event.Feedback) error { return nil },
		Withdraw: func(_ context.Context, _ string, eventID, source string, b event.Feedback) error {
			if !b.Removed || source != fmt.Sprintf("github:reaction:%d", id) || b.FindingID != "f" {
				t.Fatalf("withdrawal %s %+v", source, b)
			}
			removed = append(removed, eventID)
			if fail {
				fail = false
				return errors.New("ack lost")
			}
			return nil
		},
	}}
	c, _ := New(cfg)
	if _, err := c.PollFeedback(t.Context(), target, 1); err != nil {
		t.Fatal(err)
	}
	if err := api.RemoveReaction(t.Context(), target, id); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PollFeedback(t.Context(), target, 1); err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatal("single absence removed rating")
	}
	c, _ = New(cfg)
	if _, err := c.PollFeedback(t.Context(), target, 1); err == nil {
		t.Fatal("callback failure lost")
	}
	c, _ = New(cfg)
	if _, err := c.PollFeedback(t.Context(), target, 1); err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 || removed[0] != removed[1] {
		t.Fatalf("retry identity: %v", removed)
	}
	if _, err := c.PollFeedback(t.Context(), target, 1); err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Fatal("committed removal repeated")
	}
}

func TestPartialReactionPagesCannotWithdrawAnExistingReaction(t *testing.T) {
	api := NewFake()
	target := address.MustParse("github://acme/repo/pull/7?comment=10")
	var last int64
	for i := 0; i < 101; i++ {
		last, _ = api.SeedReaction(target, "+1", fmt.Sprint("user", i), "User")
	}
	removed := 0
	c, _ := New(Config{API: api, Tenant: "t", Feedback: connectors.FeedbackHandler{
		Resolve: func(context.Context, string, address.Address) (connectors.FeedbackTarget, error) {
			return connectors.FeedbackTarget{Found: true, Run: "r"}, nil
		},
		Record:   func(context.Context, string, string, event.Feedback) error { return nil },
		Withdraw: func(context.Context, string, string, string, event.Feedback) error { removed++; return nil },
	}})
	for range 2 {
		if n, e := c.PollFeedback(t.Context(), target, 1); n != 2 || e != nil {
			t.Fatal(n, e)
		}
		if n, e := c.PollFeedback(t.Context(), target, 2); n != 0 || e != nil {
			t.Fatal(n, e)
		}
	}
	_ = api.RemoveReaction(t.Context(), target, last)
	if n, e := c.PollFeedback(t.Context(), target, 1); n != 2 || e != nil {
		t.Fatal(n, e)
	}
	api.Err = errors.New("read failed")
	if _, e := c.PollFeedback(t.Context(), target, 2); e == nil {
		t.Fatal("missing read failure")
	}
	if removed != 0 {
		t.Fatal("partial/failed scan withdrew a reaction")
	}
	api.Err = nil
	if n, e := c.PollFeedback(t.Context(), target, 2); n != 0 || e != nil {
		t.Fatal(n, e)
	}
	if removed != 0 {
		t.Fatal("first complete absence withdrew a reaction")
	}
	_, _ = c.PollFeedback(t.Context(), target, 1)
	_, _ = c.PollFeedback(t.Context(), target, 2)
	if removed != 1 {
		t.Fatalf("stable removal count=%d", removed)
	}
}
