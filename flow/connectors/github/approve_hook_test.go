package github

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connector"
	"github.com/urmzd/saige/flow/deliverystate"
	"github.com/urmzd/saige/flow/event"
)

// approvalCase is a pull request whose host plans an approval in Review.
type approvalCase struct {
	api    *Fake
	ref    Ref
	sha    string
	target address.Address
	env    event.Envelope
	cfg    Config
	asked  []Review
}

func newApprovalCase(t *testing.T) *approvalCase {
	t.Helper()
	a := &approvalCase{api: NewFake(), ref: Ref{Owner: "acme", Repo: "project", Number: 7}, sha: strings.Repeat("a", 40)}
	a.seedHead(t, a.sha)
	a.target = address.MustParse("github://acme/project/pull/7").WithParam("head_sha", a.sha)
	a.env = event.Envelope{Tenant: "tenant", Run: "r1", Seq: 1, Channel: a.target.String(), Data: event.Completed{Text: "clean review"}, Type: event.TypeCompleted}
	a.cfg = Config{API: a.api, Tenant: "tenant", Login: "humagen[bot]", Review: func(View, []ContextFile) (Review, bool, error) {
		return Review{CommitID: a.sha, Event: "APPROVE", Body: "No findings", Comments: []ReviewComment{{Path: "a.go", Line: 1, Body: "Note", FindingID: "f1"}}}, true, nil
	}}
	return a
}

func (a *approvalCase) seedHead(t *testing.T, sha string) {
	t.Helper()
	snapshot := Context{Files: []ContextFile{{Filename: "a.go", Patch: "@@ -1 +1 @@\n-old\n+new"}}}
	snapshot.Subject.Head.SHA = sha
	if err := a.api.SeedContext(a.ref, snapshot); err != nil {
		t.Fatal(err)
	}
}

func (a *approvalCase) hook(answer bool) func(context.Context, string, string, Review) bool {
	return func(_ context.Context, owner, repo string, review Review) bool {
		if owner != "acme" || repo != "project" {
			panic("hook asked about another repository")
		}
		a.asked = append(a.asked, review)
		return answer
	}
}

func (a *approvalCase) deliver(t *testing.T) error {
	t.Helper()
	a.cfg.DeliveryState = deliverystate.NewMemory()
	c, err := New(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c.Deliver(context.Background(), a.target, a.env)
}

func TestHostApprovalWithoutHookIsRefused(t *testing.T) {
	a := newApprovalCase(t)
	if err := a.deliver(t); !errors.Is(err, ErrNoApproval) {
		t.Fatalf("planned approval with no hook answered %v, want ErrNoApproval", err)
	}
	if n := len(a.api.SubmittedReviews()); n != 0 {
		t.Fatalf("posted %d reviews with no hook", n)
	}
}

func TestHostApprovalPostsOnlyWhenHookSaysYes(t *testing.T) {
	a := newApprovalCase(t)
	a.cfg.MayApprove = a.hook(true)
	if err := a.deliver(t); err != nil {
		t.Fatal(err)
	}
	got := a.api.SubmittedReviews()
	if len(got) != 1 || got[0].Review.Event != "APPROVE" || got[0].Review.CommitID != a.sha {
		t.Fatalf("submitted %+v, want one approval on the reviewed commit", got)
	}
	if len(a.asked) != 1 || a.asked[0].Event != "APPROVE" || a.asked[0].CommitID != a.sha || strings.Contains(a.asked[0].Body, "humagen-review") {
		t.Fatalf("hook asked %+v, want the prepared approval before its markers", a.asked)
	}

	// A lost delivery record recovers the host's approval: no second review,
	// and recovery never asks the hook again.
	for range 2 {
		if err := a.deliver(t); err != nil {
			t.Fatalf("recovering the approval: %v", err)
		}
	}
	if n := len(a.api.SubmittedReviews()); n != 1 || len(a.asked) != 1 {
		t.Fatalf("recovery posted %d reviews and asked the hook %d times", n, len(a.asked))
	}
}

func TestHostApprovalHookNoPostsComment(t *testing.T) {
	a := newApprovalCase(t)
	a.cfg.MayApprove = a.hook(false)
	if err := a.deliver(t); err != nil {
		t.Fatal(err)
	}
	got := a.api.SubmittedReviews()
	if len(got) != 1 || got[0].Review.Event != "COMMENT" {
		t.Fatalf("submitted %+v, want the refused approval as one comment", got)
	}
	// Review still plans APPROVE; recovery matches the comment that was posted.
	if err := a.deliver(t); err != nil {
		t.Fatalf("recovering the comment: %v", err)
	}
	if n := len(a.api.SubmittedReviews()); n != 1 {
		t.Fatalf("recovery posted %d reviews", n)
	}
}

func TestHostApprovalRefusedWhenHeadMoved(t *testing.T) {
	t.Run("before the hook", func(t *testing.T) {
		a := newApprovalCase(t)
		a.cfg.MayApprove = a.hook(true)
		a.seedHead(t, strings.Repeat("b", 40))
		if err := a.deliver(t); !errors.Is(err, connector.ErrUndeliverable) {
			t.Fatalf("stale approval answered %v, want ErrUndeliverable", err)
		}
		if len(a.asked) != 0 || len(a.api.SubmittedReviews()) != 0 {
			t.Fatal("stale approval reached the hook or GitHub")
		}
	})
	t.Run("while the hook decides", func(t *testing.T) {
		a := newApprovalCase(t)
		a.cfg.MayApprove = func(context.Context, string, string, Review) bool {
			a.seedHead(t, strings.Repeat("b", 40))
			return true
		}
		if err := a.deliver(t); !errors.Is(err, connector.ErrUndeliverable) {
			t.Fatalf("approval after a push answered %v, want ErrUndeliverable", err)
		}
		if n := len(a.api.SubmittedReviews()); n != 0 {
			t.Fatalf("posted %d reviews on a moved head", n)
		}
	})
}
