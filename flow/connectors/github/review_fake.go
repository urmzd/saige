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
