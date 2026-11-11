package github_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connectors"
	"github.com/urmzd/saige/flow/connectors/github"
	"github.com/urmzd/saige/flow/deliverystate"
	"github.com/urmzd/saige/flow/event"
)

func TestUnresolveWithdrawsOriginalResolverWithoutErasingNewerFeedback(t *testing.T) {
	api := github.NewFake()
	ref := github.Ref{Owner: "acme", Repo: "repo", Number: 3}
	owner, comment := "", ""
	adds, withdrawals := 0, 0
	cfg := github.Config{API: api, Tenant: "t", DeliveryState: deliverystate.NewMemory(), Feedback: connectors.FeedbackHandler{
		Resolve: func(context.Context, string, address.Address) (connectors.FeedbackTarget, error) {
			return connectors.FeedbackTarget{Found: true, Run: "run", Finding: "f"}, nil
		},
		Record: func(_ context.Context, _ string, id string, b event.Feedback) error {
			if b.Removed {
				t.Fatal("removal used generic Record")
			}
			owner, comment = id, b.Comment
			adds++
			return nil
		},
		Withdraw: func(_ context.Context, _ string, id, source string, b event.Feedback) error {
			if !b.Removed || b.Actor.ID != "ada" || source == "" {
				t.Fatalf("wrong assessment owner: %s %+v", source, b)
			}
			withdrawals++
			if owner == source {
				owner, comment = "", ""
			}
			return nil
		},
	}}
	send := func(id, action, sender string, current bool) {
		t.Helper()
		api.SeedReviewThread(ref, "thread", github.ReviewThreadState{Resolved: current, ResolvedBy: "ada", Comments: []int64{5}})
		c, _ := start(t, cfg)
		body := fmt.Sprintf(`{"action":%q,"repository":{"name":"repo","owner":{"login":"acme"}},"pull_request":{"number":3,"head":{"sha":%q}},"sender":{"login":%q,"type":"User"},"thread":{"node_id":"thread","comments":[{"id":5}]}}`, action, fmt.Sprintf("%040d", adds+withdrawals), sender)
		w := httptest.NewRecorder()
		c.ServeHTTP(w, deliver(t, "pull_request_review_thread", id, body))
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", id, w.Code, w.Body.String())
		}
	}
	send("add-1", "resolved", "ada", true)
	owner, comment = "console", "new same-rating comment"
	send("remove-1", "unresolved", "bob", false)
	if owner != "console" || comment != "new same-rating comment" {
		t.Fatal("old resolution erased newer console assessment")
	}
	send("add-2", "resolved", "ada", true)
	send("delayed-remove", "unresolved", "bob", true)
	send("add-1", "resolved", "ada", true)
	if owner != "add-2:5" || adds != 2 || withdrawals != 1 {
		t.Fatalf("reordered event replaced current assessment: %q %d %d", owner, adds, withdrawals)
	}
	send("remove-2", "unresolved", "bob", false)
	send("remove-2", "unresolved", "bob", false)
	send("late-add", "resolved", "ada", false)
	if owner != "" || adds != 2 || withdrawals != 2 {
		t.Fatalf("resolution removal/replay: %q %d %d", owner, adds, withdrawals)
	}
	// A previously rejected stale delivery cannot become a fresh assessment when
	// the thread is resolved again later.
	send("late-add", "resolved", "ada", true)
	if adds != 2 {
		t.Fatal("stale event was resurrected after current state changed")
	}
}

func TestReviewThreadProofIsBoundedAndConfinedToRequestedPull(t *testing.T) {
	var number atomic.Int64
	number.Store(3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/graphql" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		fmt.Fprintf(w, `{"data":{"node":{"id":"thread","isResolved":true,"resolvedBy":{"login":"ada"},"pullRequest":{"number":%d,"repository":{"name":"repo","owner":{"login":"acme"}}},"comments":{"nodes":[{"databaseId":5}]}}}}`, number.Load())
	}))
	defer server.Close()
	api := github.NewHTTPAPIAt(server.URL, "token", server.Client())
	ref := github.Ref{Owner: "acme", Repo: "repo", Number: 3}
	got, err := api.ReviewThread(t.Context(), ref, "thread")
	if err != nil || !got.Resolved || got.ResolvedBy != "ada" || len(got.Comments) != 1 {
		t.Fatalf("proof=%+v %v", got, err)
	}
	number.Store(4)
	if _, err := api.ReviewThread(t.Context(), ref, "thread"); err == nil {
		t.Fatal("proof crossed pull request boundary")
	}
}
