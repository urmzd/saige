package github

import (
	"context"
	"fmt"
	"slices"
)

// FakeReview is a submitted native review in the in-memory API.
type FakeReview struct {
	Ref    Ref
	Review Review
	Result ReviewResult
	Files  []ContextFile
	// Author is the login that wrote the review: the app's "humagen[bot]"
	// when empty.
	Author string
	// Dismissal is the message the review was dismissed with, if it was.
	Dismissal string
}

// fakeLogin is the app login the fake writes its own reviews as.
const fakeLogin = "humagen[bot]"

// SeedReview records a review another login wrote, such as a person's, with
// its event (COMMENT, APPROVE or REQUEST_CHANGES). It returns the review ID.
func (f *Fake) SeedReview(ref Ref, author, event, commit string) (int64, error) {
	if err := validReadRef(ref, 1); err != nil {
		return 0, err
	}
	if author == "" || reviewState(event) == "" {
		return 0, fmt.Errorf("github: invalid review seed")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.submitted = append(f.submitted, FakeReview{Ref: ref, Review: Review{CommitID: commit, Event: event}, Result: ReviewResult{ID: f.nextID}, Author: author})
	return f.nextID, nil
}

func reviewState(event string) string {
	return map[string]string{"COMMENT": "COMMENTED", "APPROVE": reviewApproved, "REQUEST_CHANGES": reviewChangesRequested}[event]
}

func (entry FakeReview) record() ReviewRecord {
	v := ReviewRecord{ReviewResult: entry.Result, Body: entry.Review.Body, CommitID: entry.Review.CommitID, State: reviewState(entry.Review.Event)}
	v.PullRequestURL = fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls/%d", entry.Ref.Owner, entry.Ref.Repo, entry.Ref.Number)
	if entry.Dismissal != "" {
		v.State = "DISMISSED"
	}
	v.User.Login = entry.Author
	if v.User.Login == "" {
		v.User.Login = fakeLogin
	}
	return v
}

// Review reads one review of a pull request.
func (f *Fake) Review(_ context.Context, ref Ref, reviewID int64) (ReviewRecord, error) {
	if err := validReadRef(ref, 1); err != nil {
		return ReviewRecord{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return ReviewRecord{}, f.Err
	}
	for _, entry := range f.submitted {
		if entry.Ref == ref && entry.Result.ID == reviewID {
			return entry.record(), nil
		}
	}
	return ReviewRecord{}, &NotFoundError{What: "review", ID: reviewID}
}

// DismissReview dismisses an approval or change request, as GitHub does; a
// comment or a dismissed review is refused.
func (f *Fake) DismissReview(_ context.Context, ref Ref, reviewID int64, message string) error {
	if err := validDismissal(ref, reviewID, message); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	for i, entry := range f.submitted {
		if entry.Ref == ref && entry.Result.ID == reviewID {
			if !dismissable(entry.record().State) {
				return fmt.Errorf("github: review %d cannot be dismissed", reviewID)
			}
			f.submitted[i].Dismissal = message
			return nil
		}
	}
	return &NotFoundError{What: "review", ID: reviewID}
}

func (f *Fake) CreateReview(ctx context.Context, ref Ref, review Review) (ReviewResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return ReviewResult{}, f.Err
	}
	if ref.Number <= 0 || review.CommitID == "" {
		return ReviewResult{}, fmt.Errorf("github: review needs a pull request and commit")
	}
	if err := reviewEventAllowed(ctx, ref, review); err != nil {
		return ReviewResult{}, err
	}
	f.nextID++
	result := ReviewResult{ID: f.nextID, URL: fmt.Sprintf("https://github.com/%s/%s/pull/%d#pullrequestreview-%d", ref.Owner, ref.Repo, ref.Number, f.nextID)}
	review.Comments = slices.Clone(review.Comments)
	f.submitted = append(f.submitted, FakeReview{Ref: ref, Review: review, Result: result, Files: slices.Clone(f.contexts[ref.String()].Files)})
	return result, nil
}
func (f *Fake) SubmittedReviews() []FakeReview {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := slices.Clone(f.submitted)
	for i := range out {
		out[i].Review.Comments = slices.Clone(out[i].Review.Comments)
		out[i].Files = slices.Clone(out[i].Files)
	}
	return out
}

var _ ReviewAPI = (*Fake)(nil)
var _ ReviewDismissAPI = (*Fake)(nil)
