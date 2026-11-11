package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connectors"
	"github.com/urmzd/saige/flow/connectors/internal/render"
	"github.com/urmzd/saige/flow/deliverystate"
	"github.com/urmzd/saige/flow/event"
)

// SuggestionCommitAPI reads immutable commit evidence for the current PR head.
// Missing or ambiguous evidence never means a suggestion was accepted.
type SuggestionCommitAPI interface {
	PullHead(context.Context, Ref) (string, error)
	SuggestionCommit(context.Context, Ref, string) (SuggestionCommit, error)
}

type CommitPerson struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}
type SuggestionCommit struct {
	SHA     string `json:"sha"`
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
	Author    CommitPerson  `json:"author"`
	Committer CommitPerson  `json:"committer"`
	Files     []ContextFile `json:"files"`
}

func (a *HTTPAPI) PullHead(ctx context.Context, ref Ref) (string, error) {
	if err := validReadRef(ref, 1); err != nil {
		return "", err
	}
	var p ContextSubject
	err := a.client.Do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/pulls/%d", ref.Owner, ref.Repo, ref.Number), nil, &p)
	return p.Head.SHA, err
}
func (a *HTTPAPI) SuggestionCommit(ctx context.Context, ref Ref, sha string) (SuggestionCommit, error) {
	if err := validReadRef(ref, 1); err != nil {
		return SuggestionCommit{}, err
	}
	if !validSHA(sha) {
		return SuggestionCommit{}, fmt.Errorf("github: suggestion evidence needs an immutable commit")
	}
	var out SuggestionCommit
	err := a.client.Do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/commits/%s?per_page=100&page=1", ref.Owner, ref.Repo, sha), nil, &out)
	return out, err
}

type suggestionEvidence struct {
	Commit      string `json:"commit"`
	Path        string `json:"path"`
	Start       int    `json:"start"`
	End         int    `json:"end"`
	Replacement string `json:"replacement"`
	Finding     string `json:"finding"`
	Verified    string `json:"verified,omitempty"`
}

func (c *Connector) suggestionKey(target address.Address, run string) deliverystate.Key {
	return deliverystate.Key{Tenant: c.cfg.Tenant, Scope: "suggestion:" + deliveryScope(c.cfg.Tenant, c.cfg.DeliveryScope), Run: run, Channel: target.String(), Target: target.String()}
}
func (c *Connector) rememberSuggestion(ctx context.Context, target address.Address, comment ReviewComment, commit string, v render.View) error {
	const open = "```suggestion\n"
	if comment.Side != "RIGHT" || !validSHA(commit) || strings.Count(comment.Body, open) != 1 {
		return nil
	}
	_, text, _ := strings.Cut(comment.Body, open)
	replacement, _, ok := strings.Cut(text, "\n```")
	if !ok || len(replacement) > 256000 || strings.Contains(replacement, "```") {
		return nil
	}
	start := comment.StartLine
	if start == 0 {
		start = comment.Line
	}
	if start < 1 || start > comment.Line {
		return nil
	}
	e := suggestionEvidence{Commit: commit, Path: comment.Path, Start: start, End: comment.Line, Replacement: replacement, Finding: comment.FindingID}
	return c.feedbackState.Update(ctx, c.suggestionKey(target, v.RunID), func(raw []byte) ([]byte, bool, error) {
		if len(raw) > 0 {
			return raw, false, nil
		}
		b, err := json.Marshal(e)
		return b, false, err
	})
}

// exactSuggestion accepts only a single-parent commit directly after the
// reviewed SHA, with a complete target file equal to this one replacement.
// Moved heads, outdated anchors and resolution alone are not commit evidence.
func exactSuggestion(e suggestionEvidence, commit SuggestionCommit, before, after FileContent) bool {
	if !validSHA(commit.SHA) || len(commit.Parents) != 1 || commit.Parents[0].SHA != e.Commit || before.Commit != e.Commit || after.Commit != commit.SHA || before.Path != e.Path || after.Path != e.Path {
		return false
	}
	changed := false
	for _, f := range commit.Files {
		if f.Filename == e.Path && f.Status == "modified" && f.Patch != "" {
			changed = true
		}
	}
	if !changed {
		return false
	}
	lines := strings.SplitAfter(before.Content, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if e.Start < 1 || e.End < e.Start || e.End > len(lines) {
		return false
	}
	replacement := e.Replacement
	if replacement != "" && strings.HasSuffix(lines[e.End-1], "\n") {
		replacement += "\n"
	}
	expected := strings.Join(lines[:e.Start-1], "") + replacement + strings.Join(lines[e.End:], "")
	return expected != before.Content && expected == after.Content
}
func (c *Connector) pollSuggestion(ctx context.Context, target address.Address, resolved connectors.FeedbackTarget) error {
	comment, _ := target.Param("review_comment")
	if _, err := strconv.ParseInt(comment, 10, 64); err != nil || resolved.Finding == "" || c.cfg.Feedback.Evidence == nil {
		return nil
	}
	api, ok := c.cfg.API.(SuggestionCommitAPI)
	if !ok {
		return nil
	}
	files, ok := c.cfg.API.(FileAPI)
	if !ok {
		return nil
	}
	ref, err := c.ref(target)
	if err != nil {
		return err
	}
	return c.feedbackState.Update(ctx, c.suggestionKey(target, resolved.Run), func(raw []byte) ([]byte, bool, error) {
		if len(raw) == 0 {
			return raw, false, nil
		}
		var e suggestionEvidence
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, false, err
		}
		if e.Verified != "" || e.Finding != resolved.Finding {
			return raw, false, nil
		}
		head, err := api.PullHead(ctx, ref)
		if err != nil {
			return nil, false, err
		}
		if !validSHA(head) || head == e.Commit {
			return raw, false, nil
		}
		commit, err := api.SuggestionCommit(ctx, ref, head)
		if err != nil {
			return nil, false, err
		}
		if commit.SHA != head || len(commit.Parents) != 1 || commit.Parents[0].SHA != e.Commit {
			return raw, false, nil
		}
		before, err := files.ReadFile(ctx, ref, e.Commit, e.Path)
		if err != nil {
			return nil, false, err
		}
		after, err := files.ReadFile(ctx, ref, head, e.Path)
		if err != nil {
			return nil, false, err
		}
		if !exactSuggestion(e, commit, before, after) {
			return raw, false, nil
		}
		current, err := api.PullHead(ctx, ref)
		if err != nil {
			return nil, false, err
		}
		if current != head {
			return raw, false, nil
		}
		b := event.Feedback{RunID: resolved.Run, SessionID: resolved.Session, MessageID: resolved.Message, FindingID: e.Finding, Actor: event.Actor{ID: "commit:" + head, Bot: true, Display: "Verified commit evidence"}, Rating: "up", Reasons: []string{"suggestion_committed"}, Comment: "Exact suggested replacement verified at https://github.com/" + ref.Owner + "/" + ref.Repo + "/commit/" + head, Surface: Name}
		id := "github:suggestion:" + comment + ":" + head
		if err := c.cfg.Feedback.Evidence(ctx, c.cfg.Tenant, id, b); err != nil {
			return nil, false, err
		}
		e.Verified = head
		data, err := json.Marshal(e)
		return data, false, err
	})
}
