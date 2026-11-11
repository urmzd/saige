package github

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/attachment"
	"github.com/urmzd/saige/flow/attachment/intake"
	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/thread"
)

// Comment is one conversation comment on an issue or pull request, as the
// API lists it with rendered bodies.
type Comment struct {
	ID        int64     `json:"id"`
	User      user      `json:"user"`
	Body      string    `json:"body"`
	BodyHTML  string    `json:"body_html"`
	CreatedAt time.Time `json:"created_at"`
}

// CommentsAPI is the optional capability to list an issue's or pull
// request's conversation comments, oldest first, with rendered bodies.
// HTTPAPI and Fake implement it. Line comments on a pull request's diff are
// not part of this conversation and are not listed.
type CommentsAPI interface {
	ListComments(ctx context.Context, ref Ref) ([]Comment, error)
}

var _ thread.Lister = (*Connector)(nil)

// Messages implements thread.Lister: the conversation comments of the issue
// or pull request surface names, oldest first, with the comment id as the
// stable id (which is what an event's comment_id carries). A comment
// by one of the logins in Config.Bots is this deployment's own. Files in a
// comment are read from its rendered body, whose signed URLs work for public
// and private repositories alike, and are fetched only when the journal first
// sees the comment.
func (c *Connector) Messages(ctx context.Context, tenant string, surface address.Address) ([]thread.Message, error) {
	api, ok := c.cfg.API.(CommentsAPI)
	if !ok {
		return nil, fmt.Errorf("github: this API cannot list comments")
	}
	if c.cfg.Tenant != "" && tenant != c.cfg.Tenant {
		return nil, fmt.Errorf("github: tenant %q is not the tenant this connector serves", tenant)
	}
	ref, err := c.ref(surface)
	if err != nil {
		return nil, err
	}
	comments, err := api.ListComments(ctx, ref)
	if err != nil {
		return nil, err
	}
	origin := address.Address{Connector: Name, Workspace: surface.Workspace, Path: surface.Path}
	out := make([]thread.Message, 0, len(comments))
	for _, cm := range comments {
		login := strings.ToLower(cm.User.Login)
		_, own := c.bots[login]
		if !own {
			_, own = c.bots[strings.TrimSuffix(login, "[bot]")]
		}
		msg := thread.Message{
			ID:    strconv.FormatInt(cm.ID, 10),
			Text:  cm.Body,
			Actor: event.Actor{ID: cm.User.Login, Bot: cm.User.Type == "Bot"},
			At:    cm.CreatedAt,
			Own:   own,
		}
		if c.fetcher != nil && !own {
			found := mediaIn(cm.BodyHTML)
			if len(found) == 0 {
				found = mediaIn(cm.Body)
			}
			if cands := c.fetchAll(found); len(cands) > 0 {
				msg.Files = func(ctx context.Context) ([]attachment.Attachment, error) {
					return intake.Collect(ctx, tenant, origin, c.cfg.Files.Caps, cands, c.logSkip(cm.ID)), nil
				}
			}
		}
		out = append(out, msg)
	}
	return out, nil
}

func (c *Connector) logSkip(id int64) func(intake.Skip) {
	log := c.cfg.Files.Log
	if log == nil {
		return nil
	}
	return func(s intake.Skip) {
		log.Warn("connector: a file in the thread was not brought in; the agent is told it could not be read",
			"connector", Name, "comment", id, "file", s.Name, "reason", s.Reason, "error", s.Err)
	}
}

// maxCommentPages bounds a listing: a hundred comments a page, so ten
// thousand comments, past which the conversation is refused rather than read
// forever.
const maxCommentPages = 100

// ListComments implements CommentsAPI.
func (a *HTTPAPI) ListComments(ctx context.Context, ref Ref) ([]Comment, error) {
	if ref.Number == 0 {
		return nil, fmt.Errorf("github: comments need an issue or pull request number")
	}
	var all []Comment
	for page := 1; page <= maxCommentPages; page++ {
		var batch []Comment
		path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments?per_page=100&page=%d", ref.Owner, ref.Repo, ref.Number, page)
		if err := a.full.Do(ctx, http.MethodGet, path, nil, &batch); err != nil {
			return nil, fmt.Errorf("github: comments on %s: %w", ref, err)
		}
		all = append(all, batch...)
		if len(batch) < 100 {
			return all, nil
		}
	}
	return nil, fmt.Errorf("github: %s has more than %d pages of comments", ref, maxCommentPages)
}

// BodyHTML implements BodyAPI.
func (a *HTTPAPI) BodyHTML(ctx context.Context, ref Ref, kind string, id int64) (string, error) {
	path, err := bodyPath(ref, kind, id)
	if err != nil {
		return "", err
	}
	var out struct {
		BodyHTML string `json:"body_html"`
	}
	if err := a.full.Do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return "", fmt.Errorf("github: rendered %s on %s: %w", kind, ref, err)
	}
	return out.BodyHTML, nil
}

var (
	_ BodyAPI     = (*HTTPAPI)(nil)
	_ CommentsAPI = (*HTTPAPI)(nil)
	_ BodyAPI     = (*Fake)(nil)
	_ CommentsAPI = (*Fake)(nil)
)

// SetBodyHTML makes BodyHTML answer html for a body.
func (f *Fake) SetBodyHTML(ref Ref, kind string, id int64, html string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bodies == nil {
		f.bodies = map[string]string{}
	}
	f.bodies[bodyKey(ref, kind, id)] = html
}

func bodyKey(ref Ref, kind string, id int64) string {
	return strings.ToLower(ref.String()) + "/" + kind + "/" + strconv.FormatInt(id, 10)
}

// BodyHTML implements BodyAPI.
func (f *Fake) BodyHTML(_ context.Context, ref Ref, kind string, id int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return "", f.Err
	}
	body, ok := f.bodies[bodyKey(ref, kind, id)]
	if !ok {
		return "", fmt.Errorf("github: rendered %s on %s: not found", kind, ref)
	}
	return body, nil
}

// ListComments implements CommentsAPI from the comments posted through the
// fake, as the app, plus any a test added with AddComment.
func (f *Fake) ListComments(_ context.Context, ref Ref) ([]Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	var out []Comment
	for _, c := range f.comments {
		if c.Ref != ref || c.ReviewRoot != 0 {
			continue
		}
		login := c.Login
		if login == "" {
			login = FakeAppLogin
		}
		out = append(out, Comment{ID: c.ID, User: user{Login: login, Type: c.UserType}, Body: c.Body, BodyHTML: c.BodyHTML})
	}
	return out, nil
}

// FakeAppLogin is the login the fake posts the app's own comments as.
const FakeAppLogin = "fiber[bot]"

// AddComment posts a comment as someone else and returns its id.
func (f *Fake) AddComment(ref Ref, login, body, bodyHTML string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.comments = append(f.comments, FakeComment{ID: f.nextID, Ref: ref, Body: body, BodyHTML: bodyHTML, Login: login, UserType: "User"})
	f.byID[f.nextID] = len(f.comments) - 1
	return f.nextID
}
