package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/urmzd/saige/flow/connector"
)

// ReviewDismissAPI reads one pull request review and dismisses it. It is
// transport only: Connector.DismissReview is the host's entry point, and it
// checks that the review is the app's own before it dismisses anything. No
// model tool reaches it.
type ReviewDismissAPI interface {
	Review(ctx context.Context, ref Ref, reviewID int64) (ReviewRecord, error)
	DismissReview(ctx context.Context, ref Ref, reviewID int64, message string) error
}

// ErrNotOwnReview refuses to dismiss a review the connector's own login did
// not write, or one GitHub cannot dismiss (a comment, or one already dismissed).
var ErrNotOwnReview = errors.New("github: only the app's own approval or change request can be dismissed")

// Review reads one review of a pull request.
func (a *HTTPAPI) Review(ctx context.Context, ref Ref, reviewID int64) (ReviewRecord, error) {
	if err := validReadRef(ref, 1); err != nil {
		return ReviewRecord{}, err
	}
	if reviewID <= 0 {
		return ReviewRecord{}, fmt.Errorf("github: review needs a positive ID")
	}
	var out ReviewRecord
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews/%d", ref.Owner, ref.Repo, ref.Number, reviewID)
	if err := a.client.Do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return ReviewRecord{}, fmt.Errorf("github: read review %d on %s: %w", reviewID, ref, err)
	}
	if out.ID != reviewID {
		return ReviewRecord{}, fmt.Errorf("github: review response did not match requested review")
	}
	return out, nil
}

// DismissReview dismisses one review with a message GitHub shows on it.
func (a *HTTPAPI) DismissReview(ctx context.Context, ref Ref, reviewID int64, message string) error {
	if err := validDismissal(ref, reviewID, message); err != nil {
		return err
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews/%d/dismissals", ref.Owner, ref.Repo, ref.Number, reviewID)
	body := struct {
		Message string `json:"message"`
		Event   string `json:"event"`
	}{message, "DISMISS"}
	if err := a.client.Do(ctx, http.MethodPut, path, body, nil); err != nil {
		return fmt.Errorf("github: dismiss review %d on %s: %w", reviewID, ref, err)
	}
	return nil
}

func validDismissal(ref Ref, reviewID int64, message string) error {
	if err := validReadRef(ref, 1); err != nil {
		return err
	}
	if reviewID <= 0 || strings.TrimSpace(message) == "" {
		return fmt.Errorf("github: a dismissal needs a positive review ID and a message")
	}
	return nil
}

// GitHub's states of a submitted review that can be dismissed.
const (
	reviewApproved         = "APPROVED"
	reviewChangesRequested = "CHANGES_REQUESTED"
)

// dismissable is GitHub's rule: only an approval or a change request can be
// dismissed.
func dismissable(state string) bool {
	return state == reviewApproved || state == reviewChangesRequested
}

// OwnReviews reads the reviews the connector's own login (Config.Login) wrote
// on a pull request: each with its ID, state, commit and author. It reads at
// most 1000 reviews and fails beyond that. With Config.Login empty it fails:
// no review is the app's own.
func (c *Connector) OwnReviews(ctx context.Context, owner, repo string, number int) ([]ReviewRecord, error) {
	ref, err := c.hostPull(owner, repo, number)
	if err != nil {
		return nil, err
	}
	api, ok := c.cfg.API.(ReviewHistoryAPI)
	if !ok {
		return nil, fmt.Errorf("%w: GitHub review history", connector.ErrUnsupported)
	}
	var out []ReviewRecord
	for page := 1; ; page++ {
		rows, err := api.ListReviews(ctx, ref, page)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if strings.EqualFold(row.User.Login, c.cfg.Login) {
				out = append(out, row)
			}
		}
		if len(rows) < 100 {
			return out, nil
		}
		if page == 10 {
			return nil, fmt.Errorf("github: pull request has more than 1000 reviews")
		}
	}
}

// DismissReview dismisses the app's own approval or change request on a pull
// request, such as an approval left on an older head after a push, or a
// change request a later clean review clears. It reads the review first and
// refuses with ErrNotOwnReview when Config.Login did not write it, or when it
// is a comment or already dismissed. It is for the host: no model tool calls it.
func (c *Connector) DismissReview(ctx context.Context, owner, repo string, number int, reviewID int64, message string) error {
	ref, err := c.hostPull(owner, repo, number)
	if err != nil {
		return err
	}
	if err := validDismissal(ref, reviewID, message); err != nil {
		return err
	}
	api, ok := c.cfg.API.(ReviewDismissAPI)
	if !ok {
		return fmt.Errorf("%w: GitHub review dismissal", connector.ErrUnsupported)
	}
	review, err := api.Review(ctx, ref, reviewID)
	if err != nil {
		return err
	}
	if !strings.EqualFold(review.User.Login, c.cfg.Login) || !dismissable(review.State) {
		return fmt.Errorf("%w: review %d is %s by %q", ErrNotOwnReview, reviewID, review.State, review.User.Login)
	}
	return api.DismissReview(ctx, ref, reviewID, message)
}

// hostPull checks a host's pull request reference the way a delivery address is
// checked: a served owner and a valid repository, and a login to act as.
func (c *Connector) hostPull(owner, repo string, number int) (Ref, error) {
	if strings.TrimSpace(c.cfg.Login) == "" {
		return Ref{}, fmt.Errorf("%w: Config.Login names no app", ErrNotOwnReview)
	}
	ref, err := c.ref(refAddress(Ref{Owner: owner, Repo: repo, Number: number}, "pull"))
	if err != nil {
		return Ref{}, err
	}
	return ref, validReadRef(ref, 1)
}

var _ ReviewDismissAPI = (*HTTPAPI)(nil)
