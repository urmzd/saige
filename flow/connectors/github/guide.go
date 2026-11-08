package github

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connectors/mention"
)

// GuideAPI maintains a PR-scoped guide separately from run-scoped answers.
// The implementation must refuse an update if the PR no longer has HeadSHA.
type GuideAPI interface {
	UpdateGuide(context.Context, Ref, Guide) error
}

// Guide carries a stable identity and the current commit's navigational guide.
// BotLogin is the authenticated bot's login, used when recovering its comment.
type Guide struct{ Agent, BotLogin, HeadSHA, Body string }

func guideMarker(agent string) string { return "<!-- fiber:pr-guide:" + agent + " -->" }

// UpdateGuide finds the bot-authored guide even after a restart. A human copying
// its hidden marker cannot make the connector overwrite the human's comment.
func (a *HTTPAPI) UpdateGuide(ctx context.Context, ref Ref, guide Guide) error {
	if err := validGuide(ref, guide); err != nil {
		return err
	}
	marker := guideMarker(guide.Agent)
	var found *ContextComment
	for page := 1; ; page++ {
		var comments []ContextComment
		path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments?per_page=100&page=%d", ref.Owner, ref.Repo, ref.Number, page)
		if err := a.client.Do(ctx, http.MethodGet, path, nil, &comments); err != nil {
			return fmt.Errorf("github: find PR guide: %w", err)
		}
		for i := range comments {
			candidate := &comments[i]
			if strings.EqualFold(strings.TrimSuffix(candidate.User.Login, "[bot]"), strings.TrimSuffix(guide.BotLogin, "[bot]")) && strings.HasPrefix(candidate.Body, marker+"\n") {
				found = candidate
				break
			}
		}
		if found != nil || len(comments) < 100 {
			break
		}
	}
	// Check after searching, immediately before the write. The connector also
	// serializes guide updates so older deliveries cannot win within a process.
	var current ContextSubject
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", ref.Owner, ref.Repo, ref.Number)
	if err := a.client.Do(ctx, http.MethodGet, path, nil, &current); err != nil {
		return fmt.Errorf("github: read guide head: %w", err)
	}
	if current.Head.SHA != guide.HeadSHA {
		return fmt.Errorf("github: PR guide head changed from %s to %s", guide.HeadSHA, current.Head.SHA)
	}
	body := marker + "\n" + guide.Body
	if found != nil {
		if found.Body == body {
			return nil
		}
		return a.UpdateComment(ctx, ref, found.ID, body)
	}
	_, err := a.CreateComment(ctx, ref, body)
	return err
}

func validGuide(ref Ref, guide Guide) error {
	if err := validReadRef(ref, 1); err != nil {
		return err
	}
	if !validSHA(guide.HeadSHA) || !mention.Valid(guide.Agent) || address.ValidWorkspace(strings.TrimSuffix(guide.BotLogin, "[bot]")) != nil || strings.TrimSpace(guide.Body) == "" {
		return fmt.Errorf("github: guide needs an agent, bot login, head SHA and body")
	}
	if len(guide.Body)+len(guideMarker(guide.Agent))+1 > MaxBodyChars {
		return fmt.Errorf("github: PR guide exceeds comment limit")
	}
	return nil
}

// UpdateGuide stores a single guide and compares the seeded current PR head.
func (f *Fake) UpdateGuide(_ context.Context, ref Ref, guide Guide) error {
	if err := validGuide(ref, guide); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	current, ok := f.contexts[ref.String()]
	if !ok || current.Subject.Head.SHA != guide.HeadSHA {
		return fmt.Errorf("github: PR guide head changed")
	}
	body := guideMarker(guide.Agent) + "\n" + guide.Body
	for i := range f.comments {
		comment := &f.comments[i]
		if comment.Ref == ref && comment.GuideAgent == guide.Agent && comment.GuideLogin == guide.BotLogin {
			if comment.Body != body {
				comment.Body = body
				comment.Edits++
			}
			return nil
		}
	}
	f.nextID++
	f.comments = append(f.comments, FakeComment{ID: f.nextID, Ref: ref, Body: body, GuideAgent: guide.Agent, GuideLogin: guide.BotLogin})
	f.byID[f.nextID] = len(f.comments) - 1
	return nil
}

var _ GuideAPI = (*HTTPAPI)(nil)
var _ GuideAPI = (*Fake)(nil)
