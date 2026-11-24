package github

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connector"
	"github.com/urmzd/saige/flow/deliverystate"
	"github.com/urmzd/saige/flow/event"
)

// approvalCase is an open pull request whose host plans an approval in Review.
type approvalCase struct {
	api    *Fake
	ref    Ref
	sha    string
	target address.Address
	env    event.Envelope
	cfg    Config
	asked  []ApprovalRequest
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

// seedSubject sets the pull request's head, state and draft flag.
func (a *approvalCase) seedSubject(t *testing.T, sha, state string, draft bool) {
	t.Helper()
	snapshot := Context{Files: []ContextFile{{Filename: "a.go", Patch: "@@ -1 +1 @@\n-old\n+new"}}}
	snapshot.Subject.Head.SHA = sha
	snapshot.Subject.State = state
	snapshot.Subject.Draft = draft
	snapshot.Subject.User.Login = "author"
	if err := a.api.SeedContext(a.ref, snapshot); err != nil {
		t.Fatal(err)
	}
}

func (a *approvalCase) seedHead(t *testing.T, sha string) { a.seedSubject(t, sha, "open", false) }

func (a *approvalCase) hook(answer bool) func(context.Context, ApprovalRequest) (bool, error) {
	return func(_ context.Context, req ApprovalRequest) (bool, error) {
		a.asked = append(a.asked, req)
		return answer, nil
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

func (a *approvalCase) approvals() int {
	n := 0
	for _, r := range a.api.SubmittedReviews() {
		if r.Review.Event == "APPROVE" {
			n++
		}
	}
	return n
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
	want := ApprovalRequest{Owner: "acme", Repo: "project", Number: 7, Commit: a.sha, Author: "author"}
	if len(a.asked) != 1 {
		t.Fatalf("hook asked %d times", len(a.asked))
	}
	req := a.asked[0]
	if req.Owner != want.Owner || req.Repo != want.Repo || req.Number != want.Number || req.Commit != want.Commit || req.Author != want.Author || req.Draft {
		t.Fatalf("hook asked %+v, want the pull request %+v", req, want)
	}
	if req.Review.Event != "APPROVE" || req.Review.CommitID != a.sha || strings.Contains(req.Review.Body, "humagen-review") {
		t.Fatalf("hook asked about %+v, want the prepared approval before its markers", req.Review)
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

// A hook error, such as a failed checks read, posts nothing and the delivery
// is retried, asking again; it never becomes a permanent comment.
func TestHostApprovalHookErrorRetries(t *testing.T) {
	a := newApprovalCase(t)
	calls := 0
	a.cfg.MayApprove = func(context.Context, ApprovalRequest) (bool, error) {
		calls++
		if calls == 1 {
			return false, errors.New("checks read failed")
		}
		return true, nil
	}
	if err := a.deliver(t); err == nil || errors.Is(err, connector.ErrUndeliverable) {
		t.Fatalf("hook error answered %v, want a retryable error", err)
	}
	if n := len(a.api.SubmittedReviews()); n != 0 {
		t.Fatalf("hook error posted %d reviews", n)
	}
	if err := a.deliver(t); err != nil {
		t.Fatal(err)
	}
	if got := a.api.SubmittedReviews(); len(got) != 1 || got[0].Review.Event != "APPROVE" || calls != 2 {
		t.Fatalf("retry posted %+v after %d hook calls, want one approval", got, calls)
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
		a.cfg.MayApprove = func(context.Context, ApprovalRequest) (bool, error) {
			a.seedHead(t, strings.Repeat("b", 40))
			return true, nil
		}
		if err := a.deliver(t); !errors.Is(err, connector.ErrUndeliverable) {
			t.Fatalf("approval after a push answered %v, want ErrUndeliverable", err)
		}
		if n := len(a.api.SubmittedReviews()); n != 0 {
			t.Fatalf("posted %d reviews on a moved head", n)
		}
	})
}

// Only an open pull request that is not a draft is approved. One that is closed
// or a draft before the hook is not asked about; one that becomes so while the
// hook decides is not approved. Its review posts as a comment either way.
func TestHostApprovalOnlyOnOpenReadyPullRequest(t *testing.T) {
	for _, tc := range []struct {
		name   string
		state  string
		draft  bool
		during bool
	}{
		{"closed", "closed", false, false},
		{"draft", "open", true, false},
		{"closed while the hook decides", "closed", false, true},
		{"draft while the hook decides", "open", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newApprovalCase(t)
			asked := 0
			a.cfg.MayApprove = func(context.Context, ApprovalRequest) (bool, error) {
				asked++
				if tc.during {
					a.seedSubject(t, a.sha, tc.state, tc.draft)
				}
				return true, nil
			}
			if !tc.during {
				a.seedSubject(t, a.sha, tc.state, tc.draft)
			}
			if err := a.deliver(t); err != nil {
				t.Fatal(err)
			}
			got := a.api.SubmittedReviews()
			if len(got) != 1 || got[0].Review.Event != "COMMENT" {
				t.Fatalf("submitted %+v, want one comment", got)
			}
			if tc.during != (asked == 1) {
				t.Fatalf("hook asked %d times", asked)
			}
		})
	}
}

// No argument shape sent through the model's tool approves, even with the
// hook set to yes and a grant on the tool's context, and the hook is not asked.
func TestModelToolNeverApprovesWhateverItsArguments(t *testing.T) {
	a := newApprovalCase(t)
	asked := 0
	a.cfg.MayApprove = func(context.Context, ApprovalRequest) (bool, error) { asked++; return true, nil }
	c, err := New(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	var invoke func(context.Context, address.Address, json.RawMessage) (connector.Result, error)
	for _, action := range c.Actions() {
		if action.Name == "github_create_review" {
			invoke = action.Invoke
		}
	}
	if invoke == nil {
		t.Fatal("no github_create_review action")
	}
	sha := a.sha
	cases := []string{
		`{"commit_id":"` + sha + `","event":"APPROVE","body":"x"}`,
		`{"commit_id":"` + sha + `","event":"approve","body":"x"}`,
		`{"commit_id":"` + sha + `","event":"Approve","body":"x"}`,
		`{"commit_id":"` + sha + `","Event":"APPROVE","body":"x"}`,
		`{"commit_id":"` + sha + `","event":"COMMENT","EVENT":"APPROVE","body":"x"}`,
		`{"commit_id":"` + sha + `","event":"COMMENT","approve":true,"body":"x"}`,
		`{"commit_id":"` + sha + `","event":"COMMENT","state":"APPROVED","body":"x"}`,
		`{"commit_id":"` + sha + `","event":" APPROVE","body":"x"}`,
		`{"commit_id":"` + sha + `","event":"APPROVE","body":"x"}`,
		`{"commit_id":"` + sha + `","event":"COMMENT","body":"x"}{"event":"APPROVE"}`,
		`{"address":"github://acme/project/pull/8","commit_id":"` + sha + `","event":"APPROVE","body":"x"}`,
		`{"commit_id":"` + sha + `","event":"COMMENT","body":"x","comments":[{"path":"a.go","line":1,"body":"y","event":"APPROVE"}]}`,
	}
	target := address.MustParse("github://acme/project/pull/7")
	for _, raw := range cases {
		for _, ctx := range []context.Context{context.Background(), withApproval(context.Background(), a.ref, sha)} {
			_, _ = invoke(ctx, target, json.RawMessage(raw))
		}
	}
	if n := a.approvals(); n != 0 {
		t.Fatalf("the tool posted %d approvals", n)
	}
	if asked != 0 {
		t.Fatalf("the tool path asked the hook %d times", asked)
	}
}

// A grant posts one approval, for its own pull request and commit only.
func TestApprovalGrantIsSingleUse(t *testing.T) {
	api := NewFake()
	ref := Ref{Owner: "acme", Repo: "project", Number: 7}
	ctx := withApproval(context.Background(), ref, "c1")
	approve := Review{CommitID: "c1", Event: "APPROVE", Body: "ok"}
	if _, err := api.CreateReview(context.Background(), Ref{Owner: "ACME", Repo: "project", Number: 7}, approve); !errors.Is(err, ErrNoApproval) {
		t.Fatalf("another ref spelling answered %v", err)
	}
	if _, err := api.CreateReview(ctx, ref, approve); err != nil {
		t.Fatal(err)
	}
	if _, err := api.CreateReview(ctx, ref, approve); !errors.Is(err, ErrNoApproval) {
		t.Fatalf("a spent grant answered %v, want ErrNoApproval", err)
	}
	if _, err := (&HTTPAPI{}).CreateReview(ctx, ref, approve); !errors.Is(err, ErrNoApproval) {
		t.Fatalf("a spent grant reached the HTTP API: %v", err)
	}
	if n := len(api.SubmittedReviews()); n != 1 {
		t.Fatalf("one grant posted %d approvals", n)
	}
}

// Review planning a spelling other than APPROVE never approves and never asks.
func TestHostPlansNonCanonicalApprove(t *testing.T) {
	for _, planned := range []string{"approve", "Approve", "APPROVED", " APPROVE"} {
		a := newApprovalCase(t)
		a.cfg.MayApprove = a.hook(true)
		a.cfg.Review = func(View, []ContextFile) (Review, bool, error) {
			return Review{CommitID: a.sha, Event: planned, Body: "x"}, true, nil
		}
		_ = a.deliver(t)
		if a.approvals() != 0 || len(a.asked) != 0 {
			t.Fatalf("%q approved or asked the hook", planned)
		}
	}
}

type flakyReviews struct {
	*Fake
	failBefore, failAfter int
}

func (f *flakyReviews) CreateReview(ctx context.Context, ref Ref, r Review) (ReviewResult, error) {
	if f.failBefore > 0 {
		f.failBefore--
		return ReviewResult{}, errors.New("connection reset")
	}
	res, err := f.Fake.CreateReview(ctx, ref, r)
	if err == nil && f.failAfter > 0 {
		f.failAfter--
		return ReviewResult{}, errors.New("response lost")
	}
	return res, err
}

// A post that fails before GitHub records it is retried and lands once; one
// whose response is lost is recovered, not posted again.
func TestHostApprovalLandsOnceAcrossPostFailures(t *testing.T) {
	for _, mode := range []string{"before", "after"} {
		a := newApprovalCase(t)
		api := &flakyReviews{Fake: a.api}
		if mode == "before" {
			api.failBefore = 1
		} else {
			api.failAfter = 1
		}
		a.cfg.API = api
		a.cfg.MayApprove = a.hook(true)
		for range 3 {
			_ = a.deliver(t)
		}
		if n := a.approvals(); n != 1 {
			t.Fatalf("%s: %d approvals", mode, n)
		}
	}
}

// With Config.Checks on, the connector's own check is still in progress when
// the hook runs. ApprovalRequest.OwnCheck names it by app and run, so
// AllPassed skips it and the commit's other checks decide.
func TestOwnCheckInProgressWhenHookRuns(t *testing.T) {
	a := newApprovalCase(t)
	a.cfg.Checks = true
	if err := a.api.SeedCommitCheck("acme", "project", a.sha, CommitCheck{Kind: CheckKindRun, Name: "ci", Status: "completed", Conclusion: "success", App: "actions"}); err != nil {
		t.Fatal(err)
	}
	var seen []CommitCheck
	var asked ApprovalRequest
	a.cfg.MayApprove = func(ctx context.Context, req ApprovalRequest) (bool, error) {
		checks, err := a.api.CommitChecks(ctx, req.Owner, req.Repo, req.Commit)
		seen, asked = checks, req
		return AllPassed(checks, req.OwnCheck), err
	}
	a.cfg.DeliveryState = deliverystate.NewMemory()
	c, err := New(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	started := event.Envelope{Tenant: "tenant", Run: "r1", Seq: 0, Channel: a.target.String(), Data: event.Accepted{}, Type: event.TypeAccepted}
	if err := c.Deliver(ctx, a.target, started); err != nil {
		t.Fatal(err)
	}
	_ = c.Flush(ctx)
	if err := c.Deliver(ctx, a.target, a.env); err != nil {
		t.Fatal(err)
	}
	if asked.RunID != "r1" || asked.App != FakeApp {
		t.Fatalf("hook asked for run %q of app %q, want r1 of %q", asked.RunID, asked.App, FakeApp)
	}
	own := 0
	for _, check := range seen {
		if asked.OwnCheck(check) {
			own++
			if check.Passed() {
				t.Fatalf("own check already passed: %+v", check)
			}
		}
	}
	if own != 1 || a.approvals() != 1 {
		t.Fatalf("hook saw %+v and %d approvals posted, want the own check skipped", seen, a.approvals())
	}
}

// Only the check of this app for this run is own. A failing check named
// fiber/... from another app, or from another run of this app on the same
// commit, still blocks; so does a status of that name.
func TestOnlyThisRunsCheckIsOwn(t *testing.T) {
	req := ApprovalRequest{RunID: "r1", App: "humagen"}
	pass := CommitCheck{Kind: CheckKindRun, Name: "ci", Status: "completed", Conclusion: "success", App: "actions"}
	mine := CommitCheck{Kind: CheckKindRun, Name: "fiber/reviewer", Status: "in_progress", App: "humagen", ExternalID: "r1"}
	for _, tc := range []struct {
		name  string
		check CommitCheck
	}{
		{"another app", CommitCheck{Kind: CheckKindRun, Name: "fiber/security-scan", Status: "completed", Conclusion: "failure", App: "scanner", ExternalID: "r1"}},
		{"another run", CommitCheck{Kind: CheckKindRun, Name: "fiber/security", Status: "completed", Conclusion: "failure", App: "humagen", ExternalID: "r0"}},
		{"no external id", CommitCheck{Kind: CheckKindRun, Name: "fiber/security", Status: "completed", Conclusion: "failure", App: "humagen"}},
		{"a status", CommitCheck{Kind: CheckKindStatus, Name: "fiber/reviewer", Status: "completed", Conclusion: "failure", App: "humagen", ExternalID: "r1"}},
	} {
		if req.OwnCheck(tc.check) {
			t.Errorf("%s: marked own", tc.name)
		}
		if AllPassed([]CommitCheck{pass, mine, tc.check}, req.OwnCheck) {
			t.Errorf("%s: a failing check was skipped", tc.name)
		}
	}
	if !AllPassed([]CommitCheck{pass, mine}, req.OwnCheck) {
		t.Fatal("this run's own check blocked the approval")
	}
	for _, empty := range []ApprovalRequest{{App: "humagen"}, {RunID: "r1"}} {
		if empty.OwnCheck(mine) {
			t.Errorf("%+v marked a check own", empty)
		}
	}
	if !req.OwnCheck(CommitCheck{Kind: CheckKindRun, Name: "fiber/reviewer", App: "Humagen", ExternalID: "r1"}) {
		t.Error("app slug compared with case")
	}
}

// Another agent's failed run on the same commit, read through the fake, blocks.
func TestOtherRunsFailedCheckBlocksThroughFake(t *testing.T) {
	a := newApprovalCase(t)
	_ = a.api.SeedCommitCheck("acme", "project", a.sha, CommitCheck{Kind: CheckKindRun, Name: "ci", Status: "completed", Conclusion: "success", App: "actions"})
	_ = a.api.SeedCommitCheck("acme", "project", a.sha, CommitCheck{Kind: CheckKindRun, Name: "fiber/security", Status: "completed", Conclusion: "failure", App: FakeApp, ExternalID: "r0"})
	got, err := a.api.CommitChecks(context.Background(), "acme", "project", a.sha)
	if err != nil {
		t.Fatal(err)
	}
	if AllPassed(got, ApprovalRequest{RunID: "r1", App: FakeApp}.OwnCheck) {
		t.Fatalf("another run's failed check was skipped: %+v", got)
	}
}
