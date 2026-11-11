package github

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connectors"
	"github.com/urmzd/saige/flow/deliverystate"
	"github.com/urmzd/saige/flow/event"
)

func TestReviewAnchorsSuggestionsAndApprovalAreValidated(t *testing.T) {
	change := "replacement"
	base := Review{CommitID: "commit", Event: "COMMENT", Body: "Findings", Comments: []ReviewComment{{Path: "new.go", Line: 1, Body: "Fix", Change: &change}, {Path: "new.go", Line: 90, Body: "Unavailable location"}}}
	out, err := PrepareReview(base, []ContextFile{{Filename: "new.go", Patch: "@@ -0,0 +1,2 @@\n+old\n+second"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Comments) != 1 || !strings.Contains(out.Comments[0].Body, "```suggestion\nreplacement") || !strings.Contains(out.Body, "Unavailable location") {
		t.Fatalf("review=%+v", out)
	}
	base.Event = "COMMENT"
	injection := "```\nunsafe"
	base.Comments[0].Change = &injection
	out, err = PrepareReview(base, []ContextFile{{Filename: "new.go", Patch: "@@ -0,0 +1,2 @@\n+old\n+second"}})
	if err != nil || len(out.Comments) != 0 {
		t.Fatalf("unapplicable suggestion emitted: %+v %v", out, err)
	}
}

// Agents comment or request changes; a person approves. Only an
// approval submitted before that rule is read back, so recovery can match it.
func TestAgentsNeverApprove(t *testing.T) {
	approve := Review{CommitID: "commit", Event: "APPROVE", Body: "Looks good"}
	if _, err := PrepareReview(approve, nil); !errors.Is(err, ErrNoApproval) {
		t.Fatalf("approval prepared: %v", err)
	}
	if _, err := prepareReview(approve, nil, true); err != nil {
		t.Fatalf("an existing approval cannot be recovered: %v", err)
	}
	api := NewFake()
	if _, err := api.CreateReview(context.Background(), Ref{Owner: "acme", Repo: "project", Number: 7}, approve); !errors.Is(err, ErrNoApproval) {
		t.Fatalf("approval submitted: %v", err)
	}
	if _, err := (&HTTPAPI{}).CreateReview(context.Background(), Ref{Owner: "acme", Repo: "project", Number: 7}, approve); !errors.Is(err, ErrNoApproval) {
		t.Fatalf("approval sent to GitHub: %v", err)
	}
	c, err := New(Config{API: api})
	if err != nil {
		t.Fatal(err)
	}
	if schema := string(c.reviewAction().Schema); strings.Contains(schema, "APPROVE") {
		t.Fatalf("review tool offers approval: %s", schema)
	}
}

func TestCompletedReviewSurvivesReplicaChangeAndLostDeliveryCommit(t *testing.T) {
	ctx := context.Background()
	api := NewFake()
	ref := Ref{Owner: "acme", Repo: "project", Number: 7}
	sha := strings.Repeat("a", 40)
	snapshot := Context{Files: []ContextFile{{Filename: "a.go", Patch: "@@ -1 +1 @@\n-old\n+new"}}}
	snapshot.Subject.Head.SHA = sha
	if err := api.SeedContext(ref, snapshot); err != nil {
		t.Fatal(err)
	}
	first := true
	planned := func(disposition string) Review {
		return Review{CommitID: sha, Event: disposition, Body: "Checked", Comments: []ReviewComment{{Path: "a.go", Line: 1, Body: "Finding", FindingID: "f1"}}}
	}
	cfg := Config{API: api, Tenant: "tenant", Login: "humagen[bot]", Review: func(View, []ContextFile) (Review, bool, error) {
		if first {
			return planned("REQUEST_CHANGES"), true, nil
		}
		r := planned("COMMENT")
		r.Body += " a later plan differs"
		return r, true, nil
	}, RecoverReview: func(_ View, files []ContextFile, disposition string) (Review, bool, error) {
		if len(files) == 0 || !strings.Contains(files[0].Patch, "+new") {
			t.Fatal("original remote diff evidence missing")
		}
		return planned(disposition), true, nil
	}}
	var observed []connectors.Delivery
	cfg.ObserveDelivery = func(_ context.Context, d connectors.Delivery) error { observed = append(observed, d); return nil }
	target := address.MustParse("github://acme/project/pull/7").WithParam("head_sha", sha)
	env := event.Envelope{Tenant: "tenant", Run: "r1", Seq: 1, Channel: target.String(), Data: event.Completed{Text: "structured review"}, Type: event.TypeCompleted}
	for i := 0; i < 3; i++ {
		// New empty state models a crash after the remote review was created but
		// before the delivery transaction committed.
		cfg.DeliveryState = deliverystate.NewMemory()
		c, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Deliver(ctx, target, env); err != nil {
			t.Fatal(err)
		}
		// The next replica must recover the existing immutable review even when
		// both the PR head and the review it would plan now have changed.
		first = false
		snapshot.Subject.Head.SHA = strings.Repeat("b", 40)
		snapshot.Files = []ContextFile{{Filename: "a.go", Patch: "@@ -1 +1 @@\n-new\n+later"}}
		if err := api.SeedContext(ref, snapshot); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(api.SubmittedReviews()); got != 1 {
		t.Fatalf("posted %d reviews", got)
	}
	findings := 0
	for _, d := range observed {
		if d.Finding == "f1" {
			findings++
			if value, _ := d.Message.Param("review_comment"); value == "" {
				t.Fatal("native finding handle missing")
			}
		}
	}
	if findings != 3 {
		t.Fatalf("finding recovery observed %d handles", findings)
	}
	snapshot.Subject.Head.SHA = strings.Repeat("b", 40)
	_ = api.SeedContext(ref, snapshot)
	c, _ := New(cfg)
	env.Run = "r2"
	if err := c.Deliver(ctx, target, env); err == nil {
		t.Fatal("stale review published")
	}
	if len(api.SubmittedReviews()) != 1 {
		t.Fatal("stale review altered remote reviews")
	}
	// Copied markers cannot recover a changed payload or different final input.
	env.Run = "r1"
	for _, change := range []string{"payload", "input"} {
		if change == "payload" {
			api.submitted[0].Review.Body = strings.Replace(api.submitted[0].Review.Body, "Checked", "Tampered", 1)
		} else {
			env.Data = event.Completed{Text: "different structured review"}
		}
		cfg.DeliveryState = deliverystate.NewMemory()
		c, _ := New(cfg)
		if err := c.Deliver(ctx, target, env); err == nil {
			t.Fatalf("%s mismatch recovered an unrelated review", change)
		}
		api.submitted[0].Review.Body = strings.Replace(api.submitted[0].Review.Body, "Tampered", "Checked", 1)
	}
	if len(api.SubmittedReviews()) != 1 {
		t.Fatal("failed recovery submitted a duplicate")
	}
}
func TestCrossSurfaceWritesNeedTheirOwnGrant(t *testing.T) {
	api := NewFake()
	c, err := New(Config{API: api, Owners: []string{"acme"}, ReadRepositories: []string{"acme/project"}})
	if err != nil {
		t.Fatal(err)
	}
	source := address.MustParse("slack://T01/C01")
	args := json.RawMessage(`{"address":"https://github.com/acme/project/issues/7","body":"Hello"}`)
	if _, err := c.comment(context.Background(), source, args); err == nil {
		t.Fatal("read grant permitted a write")
	}
	c.cfg.ActionRepositories = []string{"acme/project"}
	if _, err := c.comment(context.Background(), source, args); err != nil {
		t.Fatal(err)
	}
	if len(api.Comments()) != 1 {
		t.Fatal("granted write not delivered")
	}
}
func TestFeedbackPollingKeepsActorFindingAndStableIdentity(t *testing.T) {
	api := NewFake()
	target := address.MustParse("github://acme/project/pull/7?comment=20")
	_, _ = api.SeedReaction(target, "-1", "alice", "User")
	_, _ = api.SeedReaction(target, "+1", "humagen[bot]", "Bot")
	var ids []string
	var got []event.Feedback
	c, err := New(Config{API: api, Tenant: "tenant", Feedback: connectors.FeedbackHandler{
		Resolve: func(context.Context, string, address.Address) (connectors.FeedbackTarget, error) {
			return connectors.FeedbackTarget{Found: true, Run: "run", Finding: "f1"}, nil
		},
		Record: func(_ context.Context, tenant, id string, b event.Feedback) error {
			if tenant != "tenant" {
				t.Fatal("wrong tenant")
			}
			ids = append(ids, id)
			got = append(got, b)
			return nil
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		next, err := c.PollFeedback(context.Background(), target, 1)
		if err != nil || next != 0 {
			t.Fatalf("poll=%d %v", next, err)
		}
	}
	if len(got) != 2 || ids[0] != ids[1] || got[0].Actor.ID != "alice" || got[0].FindingID != "f1" || got[0].Rating != "down" {
		t.Fatalf("feedback=%+v ids=%v", got, ids)
	}
}
