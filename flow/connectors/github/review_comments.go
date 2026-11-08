package github

import (
	"context"
	"fmt"
	"net/http"
)

type ReviewCommentRecord struct {
	ID                int64  `json:"id"`
	Body              string `json:"body"`
	Path              string `json:"path"`
	DiffHunk          string `json:"diff_hunk"`
	CommitID          string `json:"commit_id"`
	OriginalCommitID  string `json:"original_commit_id"`
	Line              int    `json:"line"`
	OriginalLine      int    `json:"original_line"`
	Side              string `json:"side"`
	StartLine         int    `json:"start_line"`
	OriginalStartLine int    `json:"original_start_line"`
	StartSide         string `json:"start_side"`
	User              struct {
		Login string `json:"login"`
	} `json:"user"`
}

// ReviewCommentsAPI recovers the actual native handles created by a review.
type ReviewCommentsAPI interface {
	ListReviewComments(context.Context, Ref, int64, int) ([]ReviewCommentRecord, error)
}

func (a *HTTPAPI) ListReviewComments(ctx context.Context, ref Ref, review int64, page int) ([]ReviewCommentRecord, error) {
	if err := validReadRef(ref, page); err != nil {
		return nil, err
	}
	if review <= 0 {
		return nil, fmt.Errorf("github: invalid review")
	}
	var out []ReviewCommentRecord
	err := a.client.Do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews/%d/comments?per_page=100&page=%d", ref.Owner, ref.Repo, ref.Number, review, page), nil, &out)
	return out, err
}
func (f *Fake) ListReviewComments(_ context.Context, ref Ref, review int64, page int) ([]ReviewCommentRecord, error) {
	if err := validReadRef(ref, page); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	for _, r := range f.submitted {
		if r.Ref == ref && r.Result.ID == review {
			var out []ReviewCommentRecord
			for i, c := range r.Review.Comments {
				if i/100+1 == page {
					comment := ReviewCommentRecord{ID: review*1000 + int64(i) + 1, Body: c.Body, Path: c.Path, CommitID: r.Review.CommitID, OriginalCommitID: r.Review.CommitID, Line: c.Line, OriginalLine: c.Line, Side: c.Side, StartLine: c.StartLine, OriginalStartLine: c.StartLine, StartSide: c.StartSide}
					comment.User.Login = "humagen[bot]"
					for _, file := range r.Files {
						if file.Filename == c.Path {
							comment.DiffHunk = file.Patch
							break
						}
					}
					out = append(out, comment)
				}
			}
			return out, nil
		}
	}
	return nil, nil
}
