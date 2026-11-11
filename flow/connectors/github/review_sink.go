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

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connectors"
	"github.com/urmzd/saige/flow/connectors/internal/render"
)

// ReviewRecord supports recovery after a review was created but its local
// delivery record did not commit. Login prevents a person's copied marker from
// impersonating the app's own delivery.
type ReviewRecord struct {
	ReviewResult
	Body     string `json:"body"`
	CommitID string `json:"commit_id"`
	State    string `json:"state"`
	User     struct {
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
			v := ReviewRecord{ReviewResult: entry.Result, Body: entry.Review.Body, CommitID: entry.Review.CommitID}
			v.State = map[string]string{"COMMENT": "COMMENTED", "APPROVE": "APPROVED", "REQUEST_CHANGES": "CHANGES_REQUESTED"}[entry.Review.Event]
			v.User.Login = "humagen[bot]"
			all = append(all, v)
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
}

func reviewBody(v render.View) string {
	if v.Status != render.StatusDone {
		return ""
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}
func (s reviewSurface) Create(ctx context.Context, target address.Address, body string) (string, error) {
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
		return "", fmt.Errorf("github: native review needs a pinned PR head")
	}
	marker := reviewMarker(view, pinned)
	for page := 1; page <= 10; page++ {
		rows, err := s.history.ListReviews(ctx, ref, page)
		if err != nil {
			return "", err
		}
		for _, row := range rows {
			if row.CommitID == pinned && strings.EqualFold(row.User.Login, s.c.cfg.Login) && strings.Contains(row.Body, marker) {
				return s.recoverReview(ctx, ref, row, marker, view)
			}
		}
		if len(rows) < 100 {
			break
		}
		if page == 10 {
			return "", fmt.Errorf("github: review recovery exceeds 1000 reviews")
		}
	}
	var files []ContextFile
	for page := 1; page <= 10; page++ {
		got, err := s.context.ReadContext(ctx, ref, page)
		if err != nil {
			return "", err
		}
		if got.Subject.Head.SHA != pinned {
			return "", fmt.Errorf("github: reviewed head changed; refusing stale review")
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
		return "", fmt.Errorf("github: review commit differs from pinned PR head")
	}
	review, err = PrepareReview(review, files)
	if err != nil {
		return "", err
	}
	review = markFindings(review)
	review.Body += "\n\n" + marker + "\n\n" + reviewPayloadMarker(view, review)
	result, err := s.api.CreateReview(ctx, ref, review)
	if err != nil {
		return "", err
	}
	return reviewReceipt(result.ID, review, view)
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
