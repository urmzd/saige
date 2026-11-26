package github

import (
	"strings"
	"testing"

	"github.com/urmzd/saige/flow/connectors/internal/render"
)

// Recovery takes a review as the app's own only when the app's bot account
// wrote it. With Login "humagen", a person named "humagen" whose review carries
// the run's marker is not taken as the posted review, so the sink still posts;
// the app's "humagen[bot]" review is then recovered, not posted again.
func TestRecoveryOwnsOnlyTheAppsBotReview(t *testing.T) {
	a := newApprovalCase(t)
	a.cfg.Login = "humagen"
	a.cfg.Review = func(View, []ContextFile) (Review, bool, error) {
		return Review{CommitID: a.sha, Event: "COMMENT", Body: "Checked"}, true, nil
	}
	marker := reviewMarker(render.View{Tenant: "tenant", RunID: "r1"}, a.sha)
	if _, err := a.api.SeedReview(a.ref, "humagen", "COMMENT", a.sha); err != nil {
		t.Fatal(err)
	}
	a.api.mu.Lock()
	a.api.submitted[0].Review.Body = "Copied\n\n" + marker
	a.api.mu.Unlock()

	if err := a.deliver(t); err != nil {
		t.Fatal(err)
	}
	got := a.api.SubmittedReviews()
	if len(got) != 2 || got[1].Author != "" || !strings.Contains(got[1].Review.Body, marker) {
		t.Fatalf("reviews %+v, want the person's review and one posted by the app", got)
	}
	for range 2 {
		if err := a.deliver(t); err != nil {
			t.Fatalf("recovering the app's review: %v", err)
		}
	}
	if n := len(a.api.SubmittedReviews()); n != 2 {
		t.Fatalf("recovery posted again: %d reviews", n)
	}
}
