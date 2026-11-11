package github

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connector"
)

// ReplyAPI preserves GitHub review threads while the shared sink keeps one
// edited answer per run. Conversation comments continue through the main API.
type ReplyAPI interface {
	CreateReviewReply(context.Context, Ref, int64, string) (int64, error)
	UpdateReviewComment(context.Context, Ref, int64, string) error
}

// CreateReviewReply replies to a top-level review comment on the named PR.
func (a *HTTPAPI) CreateReviewReply(ctx context.Context, ref Ref, root int64, body string) (int64, error) {
	if err := validReadRef(ref, 1); err != nil {
		return 0, err
	}
	if root <= 0 {
		return 0, fmt.Errorf("github: review reply needs a positive root comment ID")
	}
	var out comment
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/comments/%d/replies", ref.Owner, ref.Repo, ref.Number, root)
	if err := a.client.Do(ctx, http.MethodPost, path, map[string]string{"body": body}, &out); err != nil {
		return 0, fmt.Errorf("github: reply on %s: %w", ref, err)
	}
	if out.ID <= 0 {
		return 0, fmt.Errorf("github: review reply returned no ID")
	}
	return out.ID, nil
}

// UpdateReviewComment edits a review reply rather than an issue comment.
func (a *HTTPAPI) UpdateReviewComment(ctx context.Context, ref Ref, id int64, body string) error {
	if err := validReadRef(ref, 1); err != nil {
		return err
	}
	if id <= 0 {
		return fmt.Errorf("github: review comment needs a positive ID")
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/comments/%d", ref.Owner, ref.Repo, id)
	if err := a.client.Do(ctx, http.MethodPatch, path, map[string]string{"body": body}, nil); err != nil {
		return fmt.Errorf("github: edit review reply: %w", err)
	}
	return nil
}

func reviewRoot(target address.Address) (int64, error) {
	raw, exists := target.Params["review_comment"]
	if !exists {
		return 0, nil
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 || len(target.Path) != 3 || target.Path[1] != "pull" {
		return 0, fmt.Errorf("%w: invalid GitHub review thread", connector.ErrUndeliverable)
	}
	return id, nil
}

// CreateReviewReply creates a reply in an existing seeded review thread.
func (f *Fake) CreateReviewReply(_ context.Context, ref Ref, root int64, body string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return 0, f.Err
	}
	found := false
	for _, comment := range f.contexts[ref.String()].ReviewComments {
		if comment.ID == root && comment.InReplyToID == 0 {
			found = true
			break
		}
	}
	if !found {
		return 0, &NotFoundError{What: "review thread", ID: root}
	}
	f.nextID++
	f.comments = append(f.comments, FakeComment{ID: f.nextID, Ref: ref, Body: body, ReviewRoot: root})
	f.byID[f.nextID] = len(f.comments) - 1
	return f.nextID, nil
}

// UpdateReviewComment preserves the thread and resource boundary.
func (f *Fake) UpdateReviewComment(_ context.Context, ref Ref, id int64, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	i, ok := f.byID[id]
	if !ok || f.comments[i].Ref != ref || f.comments[i].ReviewRoot == 0 {
		return &NotFoundError{What: "review comment", ID: id}
	}
	f.comments[i].Body = body
	f.comments[i].Edits++
	return nil
}

var _ ReplyAPI = (*HTTPAPI)(nil)
var _ ReplyAPI = (*Fake)(nil)
