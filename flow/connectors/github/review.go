package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connector"
)

// ReviewAPI submits a native pull request review, including inline suggestions.
// Hosts validate locations against the reviewed commit with PrepareReview.
type ReviewAPI interface {
	CreateReview(context.Context, Ref, Review) (ReviewResult, error)
}

// Review is GitHub's native review request. CommitID pins every comment to the
// revision actually inspected; an omitted event must never create a draft.
type Review struct {
	CommitID string          `json:"commit_id"`
	Event    string          `json:"event"`
	Body     string          `json:"body"`
	Comments []ReviewComment `json:"comments,omitempty"`
}

// ReviewComment anchors a finding to the new (RIGHT) or old (LEFT) diff side.
// Change, when present, is a replacement including an intentional empty string.
type ReviewComment struct {
	FindingID string  `json:"-"`
	Path      string  `json:"path"`
	Body      string  `json:"body"`
	Line      int     `json:"line"`
	Side      string  `json:"side"`
	StartLine int     `json:"start_line,omitempty"`
	StartSide string  `json:"start_side,omitempty"`
	Change    *string `json:"-"`
}

type ReviewResult struct {
	ID  int64  `json:"id"`
	URL string `json:"html_url"`
}

var diffHeader = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// ErrNoApproval refuses an approving review: the model comments or requests
// changes. Only the host's review sink posts an approval, and only when
// Config.MayApprove says yes.
var ErrNoApproval = errors.New("github: agents never approve pull requests")

// approvalKey carries the one approval the review sink was granted. The type is
// unexported, so nothing outside this package can put a grant on a context.
type approvalKey struct{}

type approvalGrant struct {
	ref    Ref
	commit string
	used   atomic.Bool
}

// withApproval grants one approving CreateReview for exactly this pull request
// and commit. Only the review sink calls it, after Config.MayApprove said yes.
func withApproval(ctx context.Context, ref Ref, commit string) context.Context {
	return context.WithValue(ctx, approvalKey{}, &approvalGrant{ref: ref, commit: commit})
}

// approvalGranted reports whether ctx holds the sink's grant for this review,
// and spends it: a grant posts one approval.
func approvalGranted(ctx context.Context, ref Ref, review Review) bool {
	grant, ok := ctx.Value(approvalKey{}).(*approvalGrant)
	return ok && grant.commit != "" && grant.commit == review.CommitID && grant.ref == ref && grant.used.CompareAndSwap(false, true)
}

// ApprovalRequest is what Config.MayApprove decides: the pull request as the
// review sink read it, and the review it would post as an approval. Draft is
// always false today, since the sink posts a draft's review as a comment
// without asking; it is here so a host can state the rule itself.
type ApprovalRequest struct {
	Owner, Repo string
	Number      int
	// Commit is the reviewed head, the commit an approval lands on.
	Commit string
	// Author is the login that opened the pull request.
	Author string
	// AuthorID is the numeric user ID of Author, which a rename does not
	// change. An ID of 0 is unknown and matches nobody.
	AuthorID int64
	Draft    bool
	// RunID is the run whose review this is. The connector's check for it
	// carries it as its external ID.
	RunID string
	// App is the slug of the connector's own app, from Config.Login without
	// "[bot]", in lower case. Empty when Config.Login is empty.
	App string
	// AppID is the numeric ID of the connector's own app, Config.AppID. 0
	// when Config.AppID is unset; a value below 1 is read as no ID.
	AppID int64
	// AppReviews are the outstanding reviews of the app's bot account on the
	// pull request: each approval or change request it posted that is not
	// dismissed, oldest first. Every agent posts as the same bot account, and
	// GitHub counts only a reviewer's latest approval or change request, so
	// an approval replaces any change request the account has outstanding,
	// whichever agent made it. A host attributes each review to an agent from
	// its own records (Marker, or the review ID it kept) and refuses the
	// approval while any change request remains that it cannot attribute to
	// the agent approving.
	AppReviews []AppReview
	Review     Review
}

// AppReview is one outstanding review of the app's bot account on a pull
// request.
type AppReview struct {
	ID int64
	// State is APPROVED or CHANGES_REQUESTED.
	State string
	// CommitID is the commit the review was posted on.
	CommitID string
	// Marker is the review's delivery marker, ReviewMarker of the tenant, run
	// and commit the review sink posted it for. Empty for a review the sink
	// did not post, such as one through github_create_review.
	Marker string
}

// ChangesRequested returns the outstanding change requests in AppReviews.
func (r ApprovalRequest) ChangesRequested() []AppReview {
	var out []AppReview
	for _, review := range r.AppReviews {
		if review.State == reviewChangesRequested {
			out = append(out, review)
		}
	}
	return out
}

// OwnCheck reports whether check comes from the connector's own app, from any
// of its runs and under any name. A check run or check suite is own when its
// AppID is AppID, or when its App is App: a slug names one app, and it is how
// a check whose response carries no app ID is told apart. A commit status is
// own when its Creator is the app's bot account, "<App>[bot]". An AppID below
// 1 on the request is no ID. With no ID and App empty, nothing is own.
func (r ApprovalRequest) OwnCheck(check CommitCheck) bool {
	switch check.Kind {
	case CheckKindRun, CheckKindSuite:
		return r.AppID > 0 && check.AppID == r.AppID || r.App != "" && strings.EqualFold(check.App, r.App)
	case CheckKindStatus:
		return r.App != "" && strings.EqualFold(check.Creator, r.App+"[bot]")
	}
	return false
}

// AuthoredByApp reports whether the pull request was opened by the app's bot
// account, "<App>[bot]", compared ignoring case. With App empty the account
// cannot be told apart, and it reports true.
func (r ApprovalRequest) AuthoredByApp() bool {
	return r.App == "" || strings.EqualFold(r.Author, r.App+"[bot]")
}

// conflicting reports a check run or suite whose app ID and slug disagree
// about whether it is the connector's own: one matches and the other names
// another app.
func (r ApprovalRequest) conflicting(check CommitCheck) bool {
	if check.Kind == CheckKindStatus || r.AppID <= 0 || check.AppID == 0 || r.App == "" || check.App == "" {
		return false
	}
	return (check.AppID == r.AppID) != strings.EqualFold(check.App, r.App)
}

// ChecksAllow is the check rule for an approval, from checks the host read
// itself with CommitChecks on Commit. It is true only when the request names
// the app (AppID or App), every check of the app's own is left out (OwnCheck:
// any run, any name, and the app's own result never counts), at least one
// remaining check run or status passed with the conclusion success, none of
// them is pending or failed (a check suite that has not completed is
// pending), and the list is shorter than MaxCommitChecks. A check run or
// suite of another app that concluded neutral or skipped neither satisfies
// the rule nor fails it. A check whose app ID and slug disagree about being
// own fails it.
func (r ApprovalRequest) ChecksAllow(checks []CommitCheck) bool {
	return r.ChecksAllowRequiring(checks, nil)
}

// MaxRequiredChecks bounds the checks ChecksAllowRequiring takes.
const MaxRequiredChecks = 100

// RequiredCheck names a check an approval requires and where it must come
// from: a check run named Name published by the app with AppID, or a commit
// status of context Name set by the login Creator. Exactly one of AppID and
// Creator is set. A name alone is not a requirement: anyone who can write a
// commit status or run a workflow can report a check of any name. For a check
// a workflow defines, the app is GitHub Actions, and the workflow file the
// pull request carries may define it; branch protection, or a workflow run
// from the base branch (pull_request_target), is what ties such a check to
// the base branch's definition.
type RequiredCheck struct {
	Name    string
	AppID   int64
	Creator string
}

// ChecksAllowRequiring is ChecksAllow that also requires each of required, at
// most MaxRequiredChecks, to be present: at least one check of its name from
// its source (a check run from its AppID, or a commit status from its
// Creator, compared ignoring case), and every such check passed with
// success. A requirement with an empty name, with both or neither of AppID
// and Creator, or whose source is the connector's own app fails it, and so do
// more than MaxRequiredChecks. Nil required is ChecksAllow.
func (r ApprovalRequest) ChecksAllowRequiring(checks []CommitCheck, required []RequiredCheck) bool {
	if r.AppID <= 0 && r.App == "" || len(required) > MaxRequiredChecks {
		return false
	}
	for _, check := range checks {
		if r.conflicting(check) {
			return false
		}
	}
	for _, need := range required {
		if !r.requiredPassed(checks, need) {
			return false
		}
	}
	return AllPassed(checks, r.OwnCheck)
}

// requiredPassed reports whether need is present from its source and every
// check of its name from that source passed.
func (r ApprovalRequest) requiredPassed(checks []CommitCheck, need RequiredCheck) bool {
	byApp, byCreator := need.AppID > 0, need.Creator != ""
	if need.Name == "" || byApp == byCreator || need.AppID < 0 {
		return false
	}
	if byApp && r.AppID > 0 && need.AppID == r.AppID || byCreator && r.App != "" && strings.EqualFold(need.Creator, r.App+"[bot]") {
		return false
	}
	found := false
	for _, check := range checks {
		if check.Name != need.Name || r.OwnCheck(check) {
			continue
		}
		from := byApp && check.Kind == CheckKindRun && check.AppID == need.AppID ||
			byCreator && check.Kind == CheckKindStatus && strings.EqualFold(check.Creator, need.Creator)
		if !from {
			continue
		}
		if !check.Passed() {
			return false
		}
		found = true
	}
	return found
}

// appSlug is the app slug a login names: "humagen[bot]" and "humagen" are both
// the app "humagen".
func appSlug(login string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(login)), "[bot]")
}

// PrepareReview checks every inline location against actual changed-file
// patches. Unavailable or invalid anchors degrade to the review body; they do
// not disappear. An approving review is refused with ErrNoApproval.
func PrepareReview(review Review, files []ContextFile) (Review, error) {
	return prepareReview(review, files, false)
}

// prepareReview with allowApproval accepts an APPROVE event. The review sink
// passes it after Config.MayApprove said yes, and recovery passes it to read
// back an approval already posted; neither path is reachable from a tool.
func prepareReview(review Review, files []ContextFile, allowApproval bool) (Review, error) {
	if review.CommitID == "" {
		return Review{}, fmt.Errorf("github: review needs the inspected commit")
	}
	switch review.Event {
	case "COMMENT", "REQUEST_CHANGES":
	case "APPROVE":
		if !allowApproval {
			return Review{}, ErrNoApproval
		}
	default:
		return Review{}, fmt.Errorf("github: invalid review event %q", review.Event)
	}
	if len(review.Comments) > 100 {
		return Review{}, fmt.Errorf("github: review exceeds 100 findings")
	}
	patches := map[string]map[string]map[int]bool{}
	for _, f := range files {
		patches[f.Filename] = diffLines(f.Patch)
	}
	out := review
	out.Comments = nil
	for _, comment := range review.Comments {
		if comment.Side == "" {
			comment.Side = "RIGHT"
		}
		start := comment.StartLine
		if start == 0 {
			start = comment.Line
		}
		side := comment.StartSide
		if side == "" {
			side = comment.Side
		}
		valid := comment.Line > 0 && start > 0 && start <= comment.Line && side == comment.Side && comment.Line-start <= 1000
		lines := patches[comment.Path][comment.Side]
		for line := start; valid && line <= comment.Line; line++ {
			valid = lines[line]
		}
		if comment.Change != nil && (comment.Side != "RIGHT" || strings.Contains(*comment.Change, "```")) {
			valid = false
		}
		if !valid {
			out.Body += fmt.Sprintf("\n\n**%s:%d** (location unverified)\n\n%s", comment.Path, comment.Line, comment.Body)
			if comment.Change != nil {
				out.Body += "\n\nProposed replacement (not applied):\n\n" + *comment.Change
			}
			continue
		}
		if comment.Change != nil {
			comment.Body += "\n\n```suggestion\n" + *comment.Change + "\n```"
			comment.Change = nil
		}
		if start == comment.Line {
			comment.StartLine = 0
			comment.StartSide = ""
		} else {
			comment.StartLine = start
			comment.StartSide = side
		}
		out.Comments = append(out.Comments, comment)
	}
	return out, nil
}

// reviewEventAllowed refuses an APPROVE that does not carry the review sink's
// grant for this pull request and commit, and any event GitHub would read as a
// draft.
func reviewEventAllowed(ctx context.Context, ref Ref, review Review) error {
	switch review.Event {
	case "COMMENT", "REQUEST_CHANGES":
		return nil
	case "APPROVE":
		if approvalGranted(ctx, ref, review) {
			return nil
		}
		return ErrNoApproval
	default:
		return fmt.Errorf("github: review event is required")
	}
}

func diffLines(patch string) map[string]map[int]bool {
	lines := map[string]map[int]bool{"LEFT": {}, "RIGHT": {}}
	old, next := 0, 0
	active := false
	for _, row := range strings.Split(patch, "\n") {
		if match := diffHeader.FindStringSubmatch(row); match != nil {
			old, _ = strconv.Atoi(match[1])
			next, _ = strconv.Atoi(match[2])
			active = true
			continue
		}
		if !active || row == "" {
			continue
		}
		switch row[0] {
		case ' ':
			if old > 0 && next > 0 {
				lines["LEFT"][old] = true
				lines["RIGHT"][next] = true
			}
			old++
			next++
		case '-':
			if old > 0 {
				lines["LEFT"][old] = true
			}
			old++
		case '+':
			if next > 0 {
				lines["RIGHT"][next] = true
			}
			next++
		case '\\':
		default:
			active = false
		}
	}
	return lines
}

func (a *HTTPAPI) CreateReview(ctx context.Context, ref Ref, review Review) (ReviewResult, error) {
	if ref.Number <= 0 || review.CommitID == "" {
		return ReviewResult{}, fmt.Errorf("github: review needs a pull request and commit")
	}
	if err := reviewEventAllowed(ctx, ref, review); err != nil {
		return ReviewResult{}, err
	}
	var out ReviewResult
	endpoint := fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews", ref.Owner, ref.Repo, ref.Number)
	if err := a.client.Do(ctx, http.MethodPost, endpoint, review, &out); err != nil {
		return out, fmt.Errorf("github: review %s: %w", ref, err)
	}
	if out.ID == 0 {
		return out, fmt.Errorf("github: review returned no id")
	}
	return out, nil
}

func (c *Connector) reviewAction() connector.Action {
	return connector.Action{Name: "github_create_review", Description: "Submit a pull request review on the inspected commit. Inline findings outside its diff become body text. It comments or requests changes; agents never approve.", Schema: json.RawMessage(`{"type":"object","properties":{"address":{"type":"string"},"commit_id":{"type":"string"},"event":{"type":"string","enum":["COMMENT","REQUEST_CHANGES"]},"body":{"type":"string"},"comments":{"type":"array","items":{"type":"object","properties":{"path":{"type":"string"},"body":{"type":"string"},"line":{"type":"integer"},"side":{"type":"string","enum":["LEFT","RIGHT"]},"start_line":{"type":"integer"},"start_side":{"type":"string"},"change":{"type":"string"}},"required":["path","body","line"],"additionalProperties":false}}},"required":["commit_id","event","body"],"additionalProperties":false}`), Invoke: c.createReview}
}
func (c *Connector) createReview(ctx context.Context, target address.Address, raw json.RawMessage) (connector.Result, error) {
	if target.Connector == Name {
		if _, err := c.ref(target); err != nil {
			return connector.Result{}, err
		}
	}
	api, ok := c.cfg.API.(ReviewAPI)
	if !ok {
		return connector.Result{}, fmt.Errorf("github: API cannot create reviews")
	}
	reader, ok := c.cfg.API.(ContextAPI)
	if !ok {
		return connector.Result{}, fmt.Errorf("github: API cannot validate review locations")
	}
	var input struct {
		Address  string `json:"address"`
		CommitID string `json:"commit_id"`
		Event    string `json:"event"`
		Body     string `json:"body"`
		Comments []struct {
			Path      string  `json:"path"`
			Body      string  `json:"body"`
			Line      int     `json:"line"`
			Side      string  `json:"side"`
			StartLine int     `json:"start_line"`
			StartSide string  `json:"start_side"`
			Change    *string `json:"change"`
		} `json:"comments"`
	}
	if err := decodeArguments(raw, &input); err != nil {
		return connector.Result{}, err
	}
	target, err := c.actionTarget(target, input.Address)
	if err != nil {
		return connector.Result{}, err
	}
	ref, err := c.ref(target)
	if err != nil {
		return connector.Result{}, err
	}
	if len(target.Path) != 3 || target.Path[1] != "pull" {
		return connector.Result{}, fmt.Errorf("github: reviews require a pull request")
	}
	// The review sink's markers identify the reviews it posted; a review
	// through this tool carries none.
	if carriesReviewMarker(input.Body) {
		return connector.Result{}, fmt.Errorf("github: a review body may not carry a review sink marker")
	}
	for _, v := range input.Comments {
		if carriesReviewMarker(v.Body) || v.Change != nil && carriesReviewMarker(*v.Change) {
			return connector.Result{}, fmt.Errorf("github: a review comment may not carry a review sink marker")
		}
	}
	review := Review{CommitID: input.CommitID, Event: input.Event, Body: input.Body}
	for _, v := range input.Comments {
		review.Comments = append(review.Comments, ReviewComment{Path: v.Path, Body: v.Body, Line: v.Line, Side: v.Side, StartLine: v.StartLine, StartSide: v.StartSide, Change: v.Change})
	}
	var files []ContextFile
	for page := 1; page <= 10; page++ {
		snapshot, err := reader.ReadContext(ctx, ref, page)
		if err != nil {
			return connector.Result{}, err
		}
		if snapshot.Subject.Head.SHA != review.CommitID {
			return connector.Result{}, fmt.Errorf("github: pull request head changed; inspect the new commit before reviewing")
		}
		files = append(files, snapshot.Files...)
		if snapshot.NextPage == 0 {
			break
		}
	}
	review, err = PrepareReview(review, files)
	if err != nil {
		return connector.Result{}, err
	}
	result, err := api.CreateReview(ctx, ref, review)
	if err != nil {
		return connector.Result{}, err
	}
	return connector.Result{Summary: "Submitted pull request review", Address: refAddress(ref, "pull").WithParam("review", strconv.FormatInt(result.ID, 10))}, nil
}

var _ ReviewAPI = (*HTTPAPI)(nil)
