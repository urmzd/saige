package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/deliverystate"
)

// The request carries the author's numeric ID and the app's ID.
func TestApprovalRequestCarriesAuthorIDAndAppID(t *testing.T) {
	a := newApprovalCase(t)
	snapshot := Context{Files: []ContextFile{{Filename: "a.go", Patch: "@@ -1 +1 @@\n-old\n+new"}}}
	snapshot.Subject.Head.SHA = a.sha
	snapshot.Subject.State = "open"
	snapshot.Subject.User.Login, snapshot.Subject.User.ID = "author", 4242
	if err := a.api.SeedContext(a.ref, snapshot); err != nil {
		t.Fatal(err)
	}
	a.cfg.AppID = FakeAppID
	a.cfg.MayApprove = a.hook(false)
	if err := a.deliver(t); err != nil {
		t.Fatal(err)
	}
	if len(a.asked) != 1 || a.asked[0].AuthorID != 4242 || a.asked[0].AppID != FakeAppID {
		t.Fatalf("hook asked %+v, want author ID 4242 and app ID %d", a.asked, FakeAppID)
	}
}

// An author ID GitHub reports below 1 reaches the hook as 0.
func TestApprovalRequestAuthorIDBelowOneIsZero(t *testing.T) {
	a := newApprovalCase(t)
	snapshot := Context{Files: []ContextFile{{Filename: "a.go", Patch: "@@ -1 +1 @@\n-old\n+new"}}}
	snapshot.Subject.Head.SHA = a.sha
	snapshot.Subject.State = "open"
	snapshot.Subject.User.Login, snapshot.Subject.User.ID = "author", -9
	if err := a.api.SeedContext(a.ref, snapshot); err != nil {
		t.Fatal(err)
	}
	a.cfg.MayApprove = a.hook(false)
	if err := a.deliver(t); err != nil {
		t.Fatal(err)
	}
	if len(a.asked) != 1 || a.asked[0].AuthorID != 0 {
		t.Fatalf("hook asked %+v, want author ID 0", a.asked)
	}
}

// The request lists the app's outstanding approvals and change requests, each
// with the delivery marker of the run that posted it, so a host can refuse an
// approval while a change request it cannot attribute remains.
func TestApprovalRequestListsAppReviews(t *testing.T) {
	a := newApprovalCase(t)
	ctx := context.Background()
	// An earlier run requested changes through the review sink.
	earlier := a.cfg
	earlier.Review = func(View, []ContextFile) (Review, bool, error) {
		return Review{CommitID: a.sha, Event: "REQUEST_CHANGES", Body: "Fix this"}, true, nil
	}
	c, err := New(earlier)
	if err != nil {
		t.Fatal(err)
	}
	env := a.env
	env.Run = "r0"
	if err := c.Deliver(ctx, a.target, env); err != nil {
		t.Fatal(err)
	}
	// A comment, a dismissed approval and a person's change request are not
	// outstanding reviews of the app.
	if _, err := a.api.SeedReview(a.ref, fakeLogin, "COMMENT", a.sha); err != nil {
		t.Fatal(err)
	}
	dismissed, _ := a.api.SeedReview(a.ref, fakeLogin, "APPROVE", a.sha)
	if err := a.api.DismissReview(ctx, a.ref, dismissed, "older head"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.api.SeedReview(a.ref, "humagen", "REQUEST_CHANGES", a.sha); err != nil {
		t.Fatal(err)
	}
	direct, _ := a.api.SeedReview(a.ref, fakeLogin, "REQUEST_CHANGES", a.sha)

	a.cfg.MayApprove = a.hook(false)
	if err := a.deliver(t); err != nil {
		t.Fatal(err)
	}
	if len(a.asked) != 1 {
		t.Fatalf("hook asked %d times", len(a.asked))
	}
	got := a.asked[0].AppReviews
	if len(got) != 2 {
		t.Fatalf("app reviews %+v, want the sink's change request and the direct one", got)
	}
	if got[0].State != "CHANGES_REQUESTED" || got[0].CommitID != a.sha || got[0].Marker != ReviewMarker("tenant", "r0", a.sha) {
		t.Errorf("sink review read as %+v, want its marker for run r0", got[0])
	}
	if got[1].ID != direct || got[1].Marker != "" {
		t.Errorf("direct review read as %+v, want no marker", got[1])
	}
	if n := len(a.asked[0].ChangesRequested()); n != 2 {
		t.Errorf("ChangesRequested has %d reviews, want 2", n)
	}
}

// A marker is read only where the review sink writes it: just before the
// payload marker at the end of the body.
func TestBodyReviewMarkerReadsOnlyTheSinksPlace(t *testing.T) {
	marker := ReviewMarker("tenant", "r1", strings.Repeat("a", 40))
	payload := "<!-- humagen-review-payload:" + strings.Repeat("0", 64) + " -->"
	for _, tc := range []struct {
		body, want string
	}{
		{"text\n\n" + marker + "\n\n" + payload, marker},
		{marker + "\n\ntext", ""},
		{"text\n\n" + marker, ""},
		{"text\n\n" + marker + "\n\n" + payload + "\nmore", ""},
	} {
		if got := bodyReviewMarker(tc.body); got != tc.want {
			t.Errorf("bodyReviewMarker(%q) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

// A review through github_create_review carries none of the sink's markers.
func TestCreateReviewToolRefusesSinkMarkers(t *testing.T) {
	a := newApprovalCase(t)
	c, err := New(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	marker := ReviewMarker("tenant", "r1", a.sha)
	target := address.MustParse("github://acme/project/pull/7")
	for _, raw := range []map[string]any{
		{"commit_id": a.sha, "event": "REQUEST_CHANGES", "body": "x\n\n" + marker},
		{"commit_id": a.sha, "event": "COMMENT", "body": "x", "comments": []map[string]any{{"path": "a.go", "line": 1, "body": "<!-- humagen-review-payload:00 -->"}}},
		{"commit_id": a.sha, "event": "COMMENT", "body": "x", "comments": []map[string]any{{"path": "a.go", "line": 1, "body": "y", "change": marker}}},
	} {
		body, _ := json.Marshal(raw)
		if _, err := c.createReview(context.Background(), target, body); err == nil {
			t.Errorf("review %s was posted", body)
		}
	}
	if n := len(a.api.SubmittedReviews()); n != 0 {
		t.Fatalf("posted %d reviews", n)
	}
	body, _ := json.Marshal(map[string]any{"commit_id": a.sha, "event": "COMMENT", "body": "<!-- a note -->"})
	if _, err := c.createReview(context.Background(), target, body); err != nil {
		t.Fatalf("a plain comment was refused: %v", err)
	}
}

// pushAfterApproval moves the head right after GitHub records an approval.
type pushAfterApproval struct {
	*Fake
	t          *testing.T
	a          *approvalCase
	dismissErr error
}

func (p *pushAfterApproval) CreateReview(ctx context.Context, ref Ref, r Review) (ReviewResult, error) {
	res, err := p.Fake.CreateReview(ctx, ref, r)
	if err == nil && r.Event == "APPROVE" {
		p.a.seedHead(p.t, strings.Repeat("b", 40))
	}
	return res, err
}

func (p *pushAfterApproval) DismissReview(ctx context.Context, ref Ref, id int64, message string) error {
	if p.dismissErr != nil {
		return p.dismissErr
	}
	return p.Fake.DismissReview(ctx, ref, id, message)
}

// An approval whose head moved after it was posted is dismissed; when the
// dismissal fails, that is logged and the delivery still completes.
func TestApprovalDismissedWhenHeadMovesAfterPost(t *testing.T) {
	for _, fail := range []bool{false, true} {
		a := newApprovalCase(t)
		api := &pushAfterApproval{Fake: a.api, t: t, a: a}
		if fail {
			api.dismissErr = errors.New("dismissal refused")
		}
		var logs bytes.Buffer
		a.cfg.API = api
		a.cfg.Log = slog.New(slog.NewTextHandler(&logs, nil))
		a.cfg.MayApprove = a.hook(true)
		if err := a.deliver(t); err != nil {
			t.Fatalf("fail=%v: %v", fail, err)
		}
		got := a.api.SubmittedReviews()
		if len(got) != 1 || got[0].Review.Event != "APPROVE" {
			t.Fatalf("fail=%v: submitted %+v, want one approval", fail, got)
		}
		if dismissed := got[0].Dismissal != ""; dismissed == fail {
			t.Errorf("fail=%v: dismissed = %v", fail, dismissed)
		}
		if !strings.Contains(logs.String(), "head moved") {
			t.Errorf("fail=%v: logged %q", fail, logs.String())
		}
	}
}

// An approval whose head did not move stands.
func TestApprovalStandsWhenHeadIsUnchanged(t *testing.T) {
	a := newApprovalCase(t)
	a.cfg.MayApprove = a.hook(true)
	if err := a.deliver(t); err != nil {
		t.Fatal(err)
	}
	if got := a.api.SubmittedReviews(); len(got) != 1 || got[0].Dismissal != "" {
		t.Fatalf("submitted %+v, want one approval left standing", got)
	}
}

func TestNewRefusesNegativeAppID(t *testing.T) {
	if _, err := New(Config{API: NewFake(), AppID: -1}); err == nil {
		t.Fatal("a negative AppID was accepted")
	}
}

// A change request the app's account posts while the hook decides, such as
// another agent's, would be replaced by the approval; the sink dismisses the
// approval instead.
func TestApprovalDismissedWhenChangesRequestedDuringHook(t *testing.T) {
	a := newApprovalCase(t)
	var other int64
	a.cfg.MayApprove = func(context.Context, ApprovalRequest) (bool, error) {
		id, err := a.api.SeedReview(a.ref, fakeLogin, "REQUEST_CHANGES", a.sha)
		other = id
		return true, err
	}
	if err := a.deliver(t); err != nil {
		t.Fatal(err)
	}
	var approval *FakeReview
	for _, r := range a.api.SubmittedReviews() {
		if r.Review.Event == "APPROVE" {
			approval = &r
		}
		if r.Result.ID == other && r.Dismissal != "" {
			t.Fatal("the change request was dismissed")
		}
	}
	if approval == nil || approval.Dismissal != changesRequestedDismissal {
		t.Fatalf("approval %+v, want it posted and dismissed for the change request", approval)
	}
}

// A change request the hook was shown is the host's decision: the approval
// stands.
func TestApprovalStandsOverChangeRequestTheHookSaw(t *testing.T) {
	a := newApprovalCase(t)
	if _, err := a.api.SeedReview(a.ref, fakeLogin, "REQUEST_CHANGES", a.sha); err != nil {
		t.Fatal(err)
	}
	a.cfg.MayApprove = a.hook(true)
	if err := a.deliver(t); err != nil {
		t.Fatal(err)
	}
	for _, r := range a.api.SubmittedReviews() {
		if r.Review.Event == "APPROVE" && r.Dismissal != "" {
			t.Fatalf("approval dismissed: %q", r.Dismissal)
		}
	}
	if len(a.asked) != 1 || len(a.asked[0].ChangesRequested()) != 1 {
		t.Fatalf("hook asked %+v, want the change request shown", a.asked)
	}
}

// readsFailAfterApproval fails the named read once an approval is posted.
type readsFailAfterApproval struct {
	*Fake
	failContext, failReviews bool
	posted                   bool
}

func (r *readsFailAfterApproval) CreateReview(ctx context.Context, ref Ref, review Review) (ReviewResult, error) {
	res, err := r.Fake.CreateReview(ctx, ref, review)
	if err == nil && review.Event == "APPROVE" {
		r.posted = true
	}
	return res, err
}

func (r *readsFailAfterApproval) ReadContext(ctx context.Context, ref Ref, page int) (Context, error) {
	if r.posted && r.failContext {
		return Context{}, errors.New("read failed")
	}
	return r.Fake.ReadContext(ctx, ref, page)
}

func (r *readsFailAfterApproval) ListReviews(ctx context.Context, ref Ref, page int) ([]ReviewRecord, error) {
	if r.posted && r.failReviews {
		return nil, errors.New("read failed")
	}
	return r.Fake.ListReviews(ctx, ref, page)
}

// When the head or the reviews cannot be read again after an approval is
// posted, the approval is dismissed.
func TestApprovalDismissedWhenReReadFails(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		failContext, failReviews bool
	}{{"head", true, false}, {"reviews", false, true}} {
		a := newApprovalCase(t)
		a.cfg.API = &readsFailAfterApproval{Fake: a.api, failContext: tc.failContext, failReviews: tc.failReviews}
		a.cfg.MayApprove = a.hook(true)
		if err := a.deliver(t); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		got := a.api.SubmittedReviews()
		if len(got) != 1 || got[0].Review.Event != "APPROVE" || got[0].Dismissal != unconfirmedDismissal {
			t.Fatalf("%s: submitted %+v, want one dismissed approval", tc.name, got)
		}
	}
}

// withoutDismissal is an API that cannot dismiss reviews.
type withoutDismissal struct{ API }

func TestNewRefusesMayApproveWithoutDismissal(t *testing.T) {
	yes := func(context.Context, ApprovalRequest) (bool, error) { return true, nil }
	if _, err := New(Config{API: withoutDismissal{NewFake()}, MayApprove: yes}); err == nil {
		t.Fatal("MayApprove was accepted with an API that cannot dismiss reviews")
	}
	if _, err := New(Config{API: withoutDismissal{NewFake()}}); err != nil {
		t.Fatalf("a Config without MayApprove was refused: %v", err)
	}
	if _, err := New(Config{API: NewFake(), MayApprove: yes}); err != nil {
		t.Fatalf("the fake was refused: %v", err)
	}
}

func TestAuthoredByApp(t *testing.T) {
	for _, tc := range []struct {
		req  ApprovalRequest
		want bool
	}{
		{ApprovalRequest{App: "humagen", Author: "humagen[bot]"}, true},
		{ApprovalRequest{App: "humagen", Author: "Humagen[BOT]"}, true},
		{ApprovalRequest{App: "humagen", Author: "humagen"}, false},
		{ApprovalRequest{App: "humagen", Author: "ada"}, false},
		{ApprovalRequest{Author: "ada"}, true},
	} {
		if got := tc.req.AuthoredByApp(); got != tc.want {
			t.Errorf("%+v: AuthoredByApp = %v, want %v", tc.req, got, tc.want)
		}
	}
}

// An AppID below 1 on the request is no ID: it neither marks a check own nor
// names the app.
func TestOwnCheckIgnoresAppIDBelowOne(t *testing.T) {
	check := CommitCheck{Kind: CheckKindRun, Name: "ci", Status: "completed", Conclusion: "success", AppID: -1}
	req := ApprovalRequest{AppID: -1}
	if req.OwnCheck(check) || req.OwnCheck(CommitCheck{Kind: CheckKindRun, Name: "ci"}) {
		t.Fatal("an AppID below 1 marked a check own")
	}
	if req.ChecksAllow([]CommitCheck{check}) {
		t.Fatal("an AppID below 1 named the app")
	}
}

// A check suite that has not completed is pending, with or without runs; a
// completed suite does not count as a pass; the app's own suite is left out.
func TestChecksAllowCountsSuites(t *testing.T) {
	req := ApprovalRequest{App: "humagen", AppID: 1001}
	success := CommitCheck{Kind: CheckKindRun, Name: "ci", Status: "completed", Conclusion: "success", App: "actions", AppID: 15368}
	suite := func(status, conclusion, app string, id int64, runs int) CommitCheck {
		return CommitCheck{Kind: CheckKindSuite, Name: app, Status: status, Conclusion: conclusion, App: app, AppID: id, Runs: runs}
	}
	for _, tc := range []struct {
		name   string
		checks []CommitCheck
		want   bool
	}{
		{"queued suite without runs", []CommitCheck{success, suite("queued", "", "scanner", 2002, 0)}, false},
		{"in-progress suite", []CommitCheck{success, suite("in_progress", "", "scanner", 2002, 1)}, false},
		{"failed suite", []CommitCheck{success, suite("completed", "failure", "scanner", 2002, 1)}, false},
		{"completed suite", []CommitCheck{success, suite("completed", "success", "actions", 15368, 1)}, true},
		{"only a completed suite", []CommitCheck{suite("completed", "success", "actions", 15368, 1)}, false},
		{"own queued suite", []CommitCheck{success, suite("queued", "", "humagen", 1001, 0)}, true},
		{"suite ID and slug disagree", []CommitCheck{success, suite("completed", "success", "humagen", 3003, 1)}, false},
	} {
		if got := req.ChecksAllow(tc.checks); got != tc.want {
			t.Errorf("%s: ChecksAllow = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestChecksAllowRequiring(t *testing.T) {
	req := ApprovalRequest{App: "humagen", AppID: 1001}
	run := func(name, conclusion, app string, id int64) CommitCheck {
		return CommitCheck{Kind: CheckKindRun, Name: name, Status: "completed", Conclusion: conclusion, App: app, AppID: id}
	}
	status := func(name, conclusion, creator string) CommitCheck {
		return CommitCheck{Kind: CheckKindStatus, Name: name, Status: "completed", Conclusion: conclusion, Creator: creator}
	}
	ci := run("ci", "success", "actions", 15368)
	lint := run("lint", "success", "actions", 15368)
	legacy := status("legacy", "success", "ci-bot")
	needCI := RequiredCheck{Name: "ci", AppID: 15368}
	needLint := RequiredCheck{Name: "lint", AppID: 15368}
	needLegacy := RequiredCheck{Name: "legacy", Creator: "ci-bot"}
	many := make([]RequiredCheck, MaxRequiredChecks+1)
	for i := range many {
		many[i] = needCI
	}
	for _, tc := range []struct {
		name   string
		req    ApprovalRequest
		checks []CommitCheck
		need   []RequiredCheck
		want   bool
	}{
		{"none required", req, []CommitCheck{ci}, nil, true},
		{"present", req, []CommitCheck{ci, lint, legacy}, []RequiredCheck{needCI, needLint, needLegacy}, true},
		{"status creator ignores case", req, []CommitCheck{ci, legacy}, []RequiredCheck{{Name: "legacy", Creator: "CI-Bot"}}, true},
		{"missing", req, []CommitCheck{ci}, []RequiredCheck{needCI, needLint}, false},
		{"same name from another app", req, []CommitCheck{ci, run("lint", "success", "other", 4004)}, []RequiredCheck{needLint}, false},
		{"same name as a status", req, []CommitCheck{ci, status("lint", "success", "pr-author")}, []RequiredCheck{needLint}, false},
		{"status from another creator", req, []CommitCheck{ci, status("legacy", "success", "pr-author")}, []RequiredCheck{needLegacy}, false},
		{"neutral", req, []CommitCheck{ci, run("lint", "neutral", "actions", 15368)}, []RequiredCheck{needLint}, false},
		{"one of two failed", req, []CommitCheck{ci, lint, run("lint", "failure", "actions", 15368)}, []RequiredCheck{needLint}, false},
		{"another app's failure of that name blocks anyway", req, []CommitCheck{ci, lint, run("lint", "failure", "other", 4004)}, []RequiredCheck{needLint}, false},
		{"name case differs", req, []CommitCheck{ci}, []RequiredCheck{{Name: "CI", AppID: 15368}}, false},
		{"own app as source", req, []CommitCheck{ci, run("review", "success", "humagen", 1001)}, []RequiredCheck{{Name: "review", AppID: 1001}}, false},
		{"own bot as creator", req, []CommitCheck{ci, status("review", "success", "humagen[bot]")}, []RequiredCheck{{Name: "review", Creator: "humagen[bot]"}}, false},
		{"no source", req, []CommitCheck{ci}, []RequiredCheck{{Name: "ci"}}, false},
		{"both sources", req, []CommitCheck{ci}, []RequiredCheck{{Name: "ci", AppID: 15368, Creator: "x"}}, false},
		{"negative app ID", req, []CommitCheck{ci}, []RequiredCheck{{Name: "ci", AppID: -1}}, false},
		{"empty name", req, []CommitCheck{ci}, []RequiredCheck{{AppID: 15368}}, false},
		{"a suite of that name", req, []CommitCheck{ci, {Kind: CheckKindSuite, Name: "scanner", Status: "completed", Conclusion: "success", App: "scanner", AppID: 2002}}, []RequiredCheck{{Name: "scanner", AppID: 2002}}, false},
		{"too many", req, []CommitCheck{ci}, many, false},
		{"at the bound", req, []CommitCheck{ci}, many[:MaxRequiredChecks], true},
		{"no app named", ApprovalRequest{}, []CommitCheck{ci}, []RequiredCheck{needCI}, false},
	} {
		if got := tc.req.ChecksAllowRequiring(tc.checks, tc.need); got != tc.want {
			t.Errorf("%s: ChecksAllowRequiring = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// deliverWith delivers once with store as the delivery state.
func (a *approvalCase) deliverWith(t *testing.T, store deliverystate.Store) error {
	t.Helper()
	a.cfg.DeliveryState = store
	c, err := New(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c.Deliver(context.Background(), a.target, a.env)
}

func (a *approvalCase) approval(t *testing.T) FakeReview {
	t.Helper()
	for _, r := range a.api.SubmittedReviews() {
		if r.Review.Event == "APPROVE" {
			return r
		}
	}
	t.Fatal("no approval posted")
	return FakeReview{}
}

// An approval whose response was lost is recovered on the retry, and
// confirmed there: a change request posted while the hook decided dismisses
// it, as it does when the response arrives.
func TestRecoveredApprovalIsConfirmed(t *testing.T) {
	a := newApprovalCase(t)
	a.cfg.API = &flakyReviews{Fake: a.api, failAfter: 1}
	asked := 0
	a.cfg.MayApprove = func(context.Context, ApprovalRequest) (bool, error) {
		asked++
		_, err := a.api.SeedReview(a.ref, fakeLogin, "REQUEST_CHANGES", a.sha)
		return true, err
	}
	store := deliverystate.NewMemory()
	if err := a.deliverWith(t, store); err == nil {
		t.Fatal("a lost response was reported as delivered")
	}
	if err := a.deliverWith(t, store); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if got := a.approval(t); got.Dismissal != changesRequestedDismissal || asked != 1 {
		t.Fatalf("approval %+v after %d hook calls, want it dismissed for the change request", got, asked)
	}
	// A further retry, with the delivery record lost, recovers the dismissed
	// approval without posting again.
	if err := a.deliverWith(t, deliverystate.NewMemory()); err != nil {
		t.Fatalf("recovering the dismissed approval: %v", err)
	}
	if n := a.approvals(); n != 1 {
		t.Fatalf("%d approvals posted", n)
	}
}

// Recovery confirms against the reviews the hook was shown when they were
// kept, and against every outstanding change request when they were not.
func TestRecoveredApprovalUsesTheReviewsTheHookSaw(t *testing.T) {
	for _, kept := range []bool{true, false} {
		a := newApprovalCase(t)
		if _, err := a.api.SeedReview(a.ref, fakeLogin, "REQUEST_CHANGES", a.sha); err != nil {
			t.Fatal(err)
		}
		a.cfg.API = &flakyReviews{Fake: a.api, failAfter: 1}
		a.cfg.MayApprove = a.hook(true)
		store := deliverystate.NewMemory()
		_ = a.deliverWith(t, store)
		if !kept {
			store = deliverystate.NewMemory()
		}
		if err := a.deliverWith(t, store); err != nil {
			t.Fatalf("kept=%v: recovery: %v", kept, err)
		}
		dismissed := a.approval(t).Dismissal != ""
		if dismissed == kept {
			t.Errorf("kept=%v: approval dismissed = %v", kept, dismissed)
		}
	}
}

// cancelAfterApproval fails reads and dismissals once their context is done,
// as HTTP calls do, and cancels the delivery right after an approval is
// recorded.
type cancelAfterApproval struct {
	*Fake
	cancel context.CancelFunc
}

func (c *cancelAfterApproval) CreateReview(ctx context.Context, ref Ref, r Review) (ReviewResult, error) {
	res, err := c.Fake.CreateReview(ctx, ref, r)
	if err == nil && r.Event == "APPROVE" {
		c.cancel()
	}
	return res, err
}

func (c *cancelAfterApproval) ReadContext(ctx context.Context, ref Ref, page int) (Context, error) {
	if err := ctx.Err(); err != nil {
		return Context{}, err
	}
	return c.Fake.ReadContext(ctx, ref, page)
}

func (c *cancelAfterApproval) ListReviews(ctx context.Context, ref Ref, page int) ([]ReviewRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.Fake.ListReviews(ctx, ref, page)
}

func (c *cancelAfterApproval) DismissReview(ctx context.Context, ref Ref, id int64, message string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.Fake.DismissReview(ctx, ref, id, message)
}

// A delivery cancelled right after the approval is posted still confirms it.
func TestApprovalConfirmedAfterDeliveryCancelled(t *testing.T) {
	a := newApprovalCase(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.cfg.API = &cancelAfterApproval{Fake: a.api, cancel: cancel}
	a.cfg.MayApprove = func(context.Context, ApprovalRequest) (bool, error) {
		_, err := a.api.SeedReview(a.ref, fakeLogin, "REQUEST_CHANGES", a.sha)
		return true, err
	}
	a.cfg.DeliveryState = deliverystate.NewMemory()
	c, err := New(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Deliver(ctx, a.target, a.env)
	if got := a.approval(t); got.Dismissal != changesRequestedDismissal {
		t.Fatalf("approval %+v, want it dismissed for the change request", got)
	}
}

// stallAfterApproval blocks the head read until its context is done once an
// approval is posted, and refuses a dismissal whose context is done.
type stallAfterApproval struct {
	*Fake
	posted atomic.Bool
}

func (s *stallAfterApproval) CreateReview(ctx context.Context, ref Ref, r Review) (ReviewResult, error) {
	res, err := s.Fake.CreateReview(ctx, ref, r)
	if err == nil && r.Event == "APPROVE" {
		s.posted.Store(true)
	}
	return res, err
}

func (s *stallAfterApproval) ReadContext(ctx context.Context, ref Ref, page int) (Context, error) {
	if s.posted.Load() {
		<-ctx.Done()
		return Context{}, ctx.Err()
	}
	return s.Fake.ReadContext(ctx, ref, page)
}

func (s *stallAfterApproval) DismissReview(ctx context.Context, ref Ref, id int64, message string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.Fake.DismissReview(ctx, ref, id, message)
}

// A read that stalls after the post runs out its own time bound, and the
// dismissal still has its own.
func TestApprovalDismissedWhenReReadStalls(t *testing.T) {
	a := newApprovalCase(t)
	a.cfg.API = &stallAfterApproval{Fake: a.api}
	a.cfg.MayApprove = func(context.Context, ApprovalRequest) (bool, error) {
		_, err := a.api.SeedReview(a.ref, fakeLogin, "REQUEST_CHANGES", a.sha)
		return true, err
	}
	a.cfg.DeliveryState = deliverystate.NewMemory()
	c, err := New(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	c.confirmTimeout = 20 * time.Millisecond
	if err := c.Deliver(context.Background(), a.target, a.env); err != nil {
		t.Fatal(err)
	}
	if got := a.approval(t); got.Dismissal != unconfirmedDismissal {
		t.Fatalf("approval %+v, want it dismissed as unconfirmed", got)
	}
}

// noNestedUpdates fails an Update that starts inside another.
type noNestedUpdates struct {
	deliverystate.Store
	depth atomic.Int32
}

func (s *noNestedUpdates) Update(ctx context.Context, key deliverystate.Key, fn func([]byte) ([]byte, bool, error)) error {
	if s.depth.Add(1) > 1 {
		s.depth.Add(-1)
		return errors.New("nested delivery state update")
	}
	defer s.depth.Add(-1)
	return s.Store.Update(ctx, key, fn)
}

// The reviews the hook was shown are kept with the delivery's own state, in
// the same update: posting, losing the response and recovering never open a
// second update inside the first.
func TestApprovalKeepsTheHookReviewsWithoutNestedUpdates(t *testing.T) {
	a := newApprovalCase(t)
	if _, err := a.api.SeedReview(a.ref, fakeLogin, "REQUEST_CHANGES", a.sha); err != nil {
		t.Fatal(err)
	}
	a.cfg.API = &flakyReviews{Fake: a.api, failAfter: 1}
	var logs bytes.Buffer
	a.cfg.Log = slog.New(slog.NewTextHandler(&logs, nil))
	a.cfg.MayApprove = a.hook(true)
	store := &noNestedUpdates{Store: deliverystate.NewMemory()}
	if err := a.deliverWith(t, store); err == nil || strings.Contains(err.Error(), "nested") {
		t.Fatalf("first delivery answered %v, want the lost response", err)
	}
	if err := a.deliverWith(t, store); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	// The hook saw the change request, so the recovered approval stands.
	if got := a.approval(t); got.Dismissal != "" {
		t.Fatalf("approval dismissed: %q", got.Dismissal)
	}
	if strings.Contains(logs.String(), "nested") {
		t.Fatalf("logged a nested update: %s", logs.String())
	}
}
