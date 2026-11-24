package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connector"
	"github.com/urmzd/saige/flow/connectors"
	"github.com/urmzd/saige/flow/connectors/internal/render"
	"github.com/urmzd/saige/flow/connectors/internal/sink"
)

// ReviewRecord supports recovery after a review was created but its local
// delivery record did not commit. Login prevents a person's copied marker from
// impersonating the app's own delivery.
type ReviewRecord struct {
	ReviewResult
	Body     string `json:"body"`
	CommitID string `json:"commit_id"`
	State    string `json:"state"`
	// PullRequestURL is the API URL of the pull request the review is on.
	PullRequestURL string `json:"pull_request_url,omitempty"`
	User           struct {
		Login string `json:"login"`
	} `json:"user"`
}

// ReviewHistoryAPI reads at most 100 reviews per explicitly selected page.
type ReviewHistoryAPI interface {
	ListReviews(context.Context, Ref, int) ([]ReviewRecord, error)
}

func (a *HTTPAPI) ListReviews(ctx context.Context, ref Ref, page int) ([]ReviewRecord, error) {
	if err := validReadRef(ref, page); err != nil {
		return nil, err
	}
	var out []ReviewRecord
	err := a.client.Do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews?per_page=100&page=%d", ref.Owner, ref.Repo, ref.Number, page), nil, &out)
	return out, err
}
func (f *Fake) ListReviews(_ context.Context, ref Ref, page int) ([]ReviewRecord, error) {
	if err := validReadRef(ref, page); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	var all []ReviewRecord
	for _, entry := range f.submitted {
		if entry.Ref == ref {
			all = append(all, entry.record())
		}
	}
	start := (page - 1) * 100
	if start >= len(all) {
		return nil, nil
	}
	return slices.Clone(all[start:min(start+100, len(all))]), nil
}

type reviewSurface struct {
	c       *Connector
	api     ReviewAPI
	history ReviewHistoryAPI
	context ContextAPI
	// dismiss withdraws a posted approval that confirmApproval finds stale.
	// New requires it whenever Config.MayApprove is set.
	dismiss ReviewDismissAPI
}

func reviewBody(v render.View) string {
	if v.Status != render.StatusDone {
		return ""
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}

// Create creates the review without a memo; the engine calls CreateMemo.
func (s reviewSurface) Create(ctx context.Context, target address.Address, body string) (string, error) {
	handle, _, err := s.CreateMemo(ctx, target, body, "")
	return handle, err
}

// CreateMemo creates the review. Its memo keeps, with this delivery's state,
// the IDs of the reviews the hook was shown before an approval was posted, so
// a retry that recovers the approval confirms it against the same set.
func (s reviewSurface) CreateMemo(ctx context.Context, target address.Address, body, memo string) (string, string, error) {
	m := &reviewMemo{in: memo, out: memo}
	handle, err := s.create(ctx, target, body, m)
	return handle, m.out, err
}

// reviewMemo is the memo a CreateMemo attempt read (in) and leaves (out).
type reviewMemo struct{ in, out string }

func (s reviewSurface) create(ctx context.Context, target address.Address, body string, m *reviewMemo) (string, error) {
	var view render.View
	if err := json.Unmarshal([]byte(body), &view); err != nil {
		return "", err
	}
	ref, err := s.c.ref(target)
	if err != nil {
		return "", err
	}
	if target.Path[1] != "pull" {
		return "none", nil
	}
	pinned, _ := target.Param("head_sha")
	if pinned == "" {
		if origin, err := address.Parse(view.Channel); err == nil {
			pinned, _ = origin.Param("head_sha")
		}
	}
	if pinned == "" {
		return "", fmt.Errorf("%w: github: native review needs a pinned PR head", connector.ErrUndeliverable)
	}
	marker := reviewMarker(view, pinned)
	row, found, err := s.postedReview(ctx, ref, pinned, marker)
	if err != nil {
		return "", err
	}
	if found {
		return s.recoverAndConfirm(ctx, ref, pinned, row, marker, view, m.in)
	}
	var files []ContextFile
	var subject ContextSubject
	for page := 1; page <= 10; page++ {
		got, err := s.context.ReadContext(ctx, ref, page)
		if err != nil {
			return "", err
		}
		if got.Subject.Head.SHA != pinned {
			return "", fmt.Errorf("%w: github: reviewed head changed; refusing stale review", connector.ErrUndeliverable)
		}
		if page == 1 {
			subject = got.Subject
		}
		files = append(files, got.Files...)
		if got.NextPage == 0 {
			break
		}
	}
	review, ok, err := s.c.cfg.Review(view, files)
	if err != nil {
		return "", err
	}
	if !ok {
		return "none", nil
	}
	if review.CommitID != pinned {
		return "", fmt.Errorf("%w: github: review commit differs from pinned PR head", connector.ErrUndeliverable)
	}
	// Only the host approves: an APPROVE planned by Review passes when
	// MayApprove is set, and posts only when it says yes.
	approve := review.Event == "APPROVE" && s.c.cfg.MayApprove != nil
	review, err = prepareReview(review, files, approve)
	if err != nil {
		return "", err
	}
	var seen []AppReview
	if approve {
		if approve, seen, err = s.mayApprove(ctx, ref, pinned, view.RunID, subject, review); err != nil {
			return "", err
		}
		if !approve {
			review.Event = "COMMENT"
		}
	}
	review = markFindings(review)
	review.Body += "\n\n" + marker + "\n\n" + reviewPayloadMarker(view, review)
	if approve {
		m.out = seenMemo(seen)
	}
	result, err := s.post(ctx, ref, review, approve, seen)
	if err != nil {
		return "", err
	}
	return reviewReceipt(result.ID, review, view)
}

// postedReview finds the review the sink already posted for marker on pinned.
func (s reviewSurface) postedReview(ctx context.Context, ref Ref, pinned, marker string) (ReviewRecord, bool, error) {
	for page := 1; page <= 10; page++ {
		rows, err := s.history.ListReviews(ctx, ref, page)
		if err != nil {
			return ReviewRecord{}, false, err
		}
		for _, row := range rows {
			if row.CommitID == pinned && ownReviewAuthor(s.c.cfg.Login, row.User.Login) && strings.Contains(row.Body, marker) {
				return row, true, nil
			}
		}
		if len(rows) < 100 {
			break
		}
		if page == 10 {
			return ReviewRecord{}, false, fmt.Errorf("github: review recovery exceeds 1000 reviews")
		}
	}
	return ReviewRecord{}, false, nil
}

// recoverAndConfirm recovers a posted review. An approval whose response was
// lost was never confirmed, so it is confirmed here.
func (s reviewSurface) recoverAndConfirm(ctx context.Context, ref Ref, pinned string, row ReviewRecord, marker string, view render.View, memo string) (string, error) {
	receipt, err := s.recoverReview(ctx, ref, row, marker, view)
	if err == nil && row.State == reviewApproved && s.dismiss != nil {
		seen, known := seenFromMemo(memo)
		s.confirmApproval(ctx, ref, pinned, row.ID, seen, known)
	}
	return receipt, err
}

type seenRecord struct {
	Reviews []int64 `json:"reviews"`
}

// seenMemo records the IDs of the reviews the hook was shown.
func seenMemo(seen []AppReview) string {
	record := seenRecord{Reviews: []int64{}}
	for _, review := range seen {
		record.Reviews = append(record.Reviews, review.ID)
	}
	raw, _ := json.Marshal(record)
	return string(raw)
}

// seenFromMemo reads what seenMemo recorded; known is false when the memo
// holds no such record.
func seenFromMemo(memo string) (seen []AppReview, known bool) {
	var record seenRecord
	if memo == "" || json.Unmarshal([]byte(memo), &record) != nil || record.Reviews == nil {
		return nil, false
	}
	for _, id := range record.Reviews {
		seen = append(seen, AppReview{ID: id})
	}
	return seen, true
}

// post submits review. An approval carries the sink's single-use grant, and
// once posted is checked with confirmApproval against seen, the app's
// outstanding reviews the hook was shown.
func (s reviewSurface) post(ctx context.Context, ref Ref, review Review, approve bool, seen []AppReview) (ReviewResult, error) {
	if !approve {
		return s.api.CreateReview(ctx, ref, review)
	}
	result, err := s.api.CreateReview(withApproval(ctx, ref, review.CommitID), ref, review)
	if err != nil {
		return result, err
	}
	s.confirmApproval(ctx, ref, review.CommitID, result.ID, seen, true)
	return result, nil
}

// confirmApproval reads the pull request and the app's reviews again once an
// approval is posted, and dismisses that approval when the head is no longer
// the approved commit, when a change request of the app is outstanding that
// the hook was not shown (another agent's, posted while the hook decided,
// which the approval would replace as the account's latest review), or when
// either read fails. The posted review stands only when both reads confirm
// it. With known false the set the hook was shown is lost, and any
// outstanding change request other than this approval dismisses it. The reads
// and the dismissal run without the delivery's cancellation, each under its
// own time bound, so a delivery cancelled after the post still confirms it,
// and a read that stalls still leaves the dismissal its own time. A
// failed dismissal is logged, and the delivery still completes: the approval
// was posted. A push after these reads is the host's to handle on
// synchronize, or GitHub's, with the branch rule that dismisses stale
// approvals.
func (s reviewSurface) confirmApproval(ctx context.Context, ref Ref, pinned string, id int64, seen []AppReview, known bool) {
	ctx = context.WithoutCancel(ctx)
	log := s.c.log().With("connector", Name, "pull_request", ref.String(), "review", id)
	reason := s.approvalStale(ctx, ref, pinned, id, seen, known)
	if reason == "" {
		return
	}
	dismissCtx, cancel := context.WithTimeout(ctx, s.c.approvalStepTimeout())
	defer cancel()
	if err := s.dismiss.DismissReview(dismissCtx, ref, id, reason); err != nil {
		log.Warn("github: could not dismiss a posted approval", "reason", reason, "error", err)
		return
	}
	log.Info("github: dismissed a posted approval", "reason", reason)
}

// approvalStale returns why a posted approval must be dismissed, or "".
func (s reviewSurface) approvalStale(ctx context.Context, ref Ref, pinned string, id int64, seen []AppReview, known bool) string {
	log := s.c.log().With("connector", Name, "pull_request", ref.String(), "review", id)
	readCtx, cancel := context.WithTimeout(ctx, s.c.approvalStepTimeout())
	got, err := s.context.ReadContext(readCtx, ref, 1)
	cancel()
	if err != nil {
		log.Warn("github: could not read the head after posting an approval", "error", err)
		return unconfirmedDismissal
	}
	if got.Subject.Head.SHA != pinned {
		return headMovedDismissal
	}
	readCtx, cancel = context.WithTimeout(ctx, s.c.approvalStepTimeout())
	reviews, err := ownReviews(readCtx, s.history, s.c.cfg.Login, ref)
	cancel()
	if err != nil {
		log.Warn("github: could not read the app's reviews after posting an approval", "error", err)
		return unconfirmedDismissal
	}
	shown := map[int64]bool{id: true}
	if known {
		for _, review := range seen {
			shown[review.ID] = true
		}
	}
	for _, review := range outstandingReviews(reviews) {
		if review.State == reviewChangesRequested && !shown[review.ID] {
			return changesRequestedDismissal
		}
	}
	return ""
}

// approvalConfirmTimeout bounds each of confirmApproval's reads and its
// dismissal.
const approvalConfirmTimeout = 30 * time.Second

// approvalStepTimeout is approvalConfirmTimeout, or the connector's own bound
// when one is set.
func (c *Connector) approvalStepTimeout() time.Duration {
	if c.confirmTimeout > 0 {
		return c.confirmTimeout
	}
	return approvalConfirmTimeout
}

// The messages GitHub shows on an approval the sink dismissed.
const (
	headMovedDismissal        = "The pull request head moved after this approval was posted; it applies to an earlier commit."
	changesRequestedDismissal = "A change request was posted while this approval was decided; the approval does not replace it."
	unconfirmedDismissal      = "The pull request could not be read again after this approval was posted."
)

// stateOpen is GitHub's state of an open issue or pull request.
const stateOpen = "open"

// approvable reports whether GitHub's view of the pull request allows an
// approval: open and not a draft.
func approvable(subject ContextSubject) bool {
	return subject.State == stateOpen && !subject.Draft
}

// mayApprove decides a planned approval. A closed or draft pull request, or a
// Config.Login that names no app, is not asked about: its review posts as a
// comment, since without the app's account neither its checks nor its
// outstanding reviews can be told apart. Otherwise the host's hook
// decides, and a yes holds only if the pull request read again is still open,
// not a draft, and on the reviewed commit: the hook may take time.
func (s reviewSurface) mayApprove(ctx context.Context, ref Ref, pinned, run string, subject ContextSubject, review Review) (bool, []AppReview, error) {
	if !approvable(subject) || appSlug(s.c.cfg.Login) == "" || s.dismiss == nil {
		return false, nil, nil
	}
	review.Comments = slices.Clone(review.Comments)
	reviews, err := ownReviews(ctx, s.history, s.c.cfg.Login, ref)
	if err != nil {
		return false, nil, err
	}
	outstanding := outstandingReviews(reviews)
	yes, err := s.c.cfg.MayApprove(ctx, ApprovalRequest{
		Owner: ref.Owner, Repo: ref.Repo, Number: ref.Number, Commit: pinned,
		Author: subject.User.Login, AuthorID: knownID(subject.User.ID), Draft: subject.Draft,
		RunID: run, App: appSlug(s.c.cfg.Login), AppID: s.c.cfg.AppID,
		AppReviews: outstanding, Review: review,
	})
	if err != nil || !yes {
		return false, nil, err
	}
	got, err := s.context.ReadContext(ctx, ref, 1)
	if err != nil {
		return false, nil, err
	}
	if got.Subject.Head.SHA != pinned {
		return false, nil, fmt.Errorf("%w: github: reviewed head changed; refusing stale review", connector.ErrUndeliverable)
	}
	return approvable(got.Subject), outstanding, nil
}

func (s reviewSurface) Update(_ context.Context, _ address.Address, _ string, _ string) error {
	// A submitted review is immutable. Late earlier events can update the status
	// comment, but cannot submit a second review for the same completed run.
	return nil
}

// Keep the exact submitted anchors in the opaque durable handle. FindingID
// never enters GitHub's API JSON, so its private mapping is stored separately.
type deliveredReview struct {
	ID                   int64
	Review               Review
	Findings             []string
	Tenant, Run, Channel string
}

func reviewReceipt(id int64, review Review, view render.View) (string, error) {
	r := deliveredReview{ID: id, Review: review, Tenant: view.Tenant, Run: view.RunID, Channel: view.Channel}
	for _, comment := range review.Comments {
		r.Findings = append(r.Findings, comment.FindingID)
	}
	b, err := json.Marshal(r)
	return string(b), err
}
func (s reviewSurface) AfterDelivery(ctx context.Context, target address.Address, handle string) error {
	if handle == "none" {
		return nil
	}
	var r deliveredReview
	if err := json.Unmarshal([]byte(handle), &r); err != nil {
		return err
	}
	if r.ID <= 0 || r.Tenant != s.c.cfg.Tenant || r.Run == "" || r.Channel == "" || len(r.Findings) != len(r.Review.Comments) {
		return fmt.Errorf("github: invalid delivered review receipt")
	}
	for i := range r.Review.Comments {
		r.Review.Comments[i].FindingID = r.Findings[i]
	}
	ref, err := s.c.ref(target)
	if err != nil {
		return err
	}
	return s.observeReview(ctx, target, ref, r.ID, r.Review, render.View{Tenant: r.Tenant, RunID: r.Run, Channel: r.Channel})
}

func findingMarker(id string) string {
	sum := sha256.Sum256([]byte(id))
	return "<!-- humagen-finding:" + hex.EncodeToString(sum[:]) + " -->"
}
func (s reviewSurface) observeReview(ctx context.Context, target address.Address, ref Ref, id int64, review Review, v render.View) error {
	if s.c.cfg.ObserveDelivery == nil {
		return nil
	}
	expected := map[string]ReviewComment{}
	for _, comment := range review.Comments {
		if comment.FindingID != "" {
			expected[findingMarker(comment.FindingID)] = comment
		}
	}
	if len(expected) == 0 {
		return nil
	}
	api, ok := s.c.cfg.API.(ReviewCommentsAPI)
	if !ok {
		return fmt.Errorf("github: native finding feedback requires review comment history")
	}
	for page := 1; page <= 10; page++ {
		comments, err := api.ListReviewComments(ctx, ref, id, page)
		if err != nil {
			return err
		}
		for _, comment := range comments {
			for marker, finding := range expected {
				if strings.Contains(comment.Body, marker) {
					message := refAddress(ref, "pull").WithParam("review_comment", fmt.Sprint(comment.ID))
					if err := s.c.rememberSuggestion(ctx, message, finding, review.CommitID, v); err != nil {
						return err
					}
					if err := s.c.cfg.ObserveDelivery(ctx, connectors.Delivery{Tenant: v.Tenant, Run: v.RunID, Channel: v.Channel, Target: message, Message: message, Finding: finding.FindingID}); err != nil {
						return err
					}
					delete(expected, marker)
				}
			}
		}
		if len(comments) < 100 {
			break
		}
	}
	if len(expected) > 0 {
		return fmt.Errorf("github: native finding comments not yet available")
	}
	return nil
}

var _ sink.MemoCreator = reviewSurface{}
