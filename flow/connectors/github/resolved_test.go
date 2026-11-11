package github_test

import (
	"context"
	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connectors"
	"github.com/urmzd/saige/flow/connectors/github"
	"github.com/urmzd/saige/flow/event"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResolutionOnlyRatesVerifiedKnownFinding(t *testing.T) {
	var got []event.Feedback
	api := github.NewFake()
	ref := github.Ref{Owner: "acme", Repo: "repo", Number: 3}
	c, tags := start(t, github.Config{API: api, Tenant: "t", Owners: []string{"acme"}, Feedback: connectors.FeedbackHandler{
		Resolve: func(_ context.Context, _ string, a address.Address) (connectors.FeedbackTarget, error) {
			id, _ := a.Param("review_comment")
			return connectors.FeedbackTarget{Found: id == "5", Run: "run", Finding: "f1"}, nil
		},
		Record: func(_ context.Context, tenant, id string, b event.Feedback) error { got = append(got, b); return nil },
		Withdraw: func(_ context.Context, tenant, id, source string, b event.Feedback) error {
			if source != "resolved:5" {
				t.Fatalf("wrong resolution source %q", source)
			}
			got = append(got, b)
			return nil
		},
	}})
	body := `{"action":"resolved","repository":{"name":"repo","full_name":"acme/repo","owner":{"login":"acme"}},"pull_request":{"number":3},"sender":{"login":"ada","type":"User"},"thread":{"node_id":"thread","comments":[{"id":5},{"id":6}]}}`
	for _, action := range []string{"resolved", "unresolved"} {
		api.SeedReviewThread(ref, "thread", github.ReviewThreadState{Resolved: action == "resolved", ResolvedBy: "ada", Comments: []int64{5, 6}})
		w := httptest.NewRecorder()
		c.ServeHTTP(w, deliver(t, "pull_request_review_thread", action, strings.Replace(body, `"resolved"`, `"`+action+`"`, 1)))
		if w.Code != http.StatusOK {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if len(got) != 2 || got[0].FindingID != "f1" || got[0].Reasons[0] != "resolved" || !got[1].Removed {
		t.Fatal(got)
	}
	select {
	case <-tags:
		t.Fatal("resolution started a model turn")
	default:
	}
	bad := deliver(t, "pull_request_review_thread", "bad", body)
	bad.Header.Set("X-Hub-Signature-256", "bad")
	c.ServeHTTP(httptest.NewRecorder(), bad)
	if len(got) != 2 {
		t.Fatal("unsigned resolution accepted")
	}
}
