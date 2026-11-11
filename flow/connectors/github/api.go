package github

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/urmzd/saige/flow/connectors/internal/httpjson"
)

// Ref names a repository resource: a repository, and optionally the issue or
// pull request inside it.
//
// One type serves both because GitHub's REST API does: a pull request is an
// issue that has a branch, and its conversation comments live under
// /issues/{number}/comments whatever the address called it.
type Ref struct {
	Owner string
	Repo  string
	// Number is the issue or pull request number. Zero names the repository.
	Number int
}

// String renders the ref the way a GitHub URL path does.
func (r Ref) String() string {
	if r.Number == 0 {
		return r.Owner + "/" + r.Repo
	}
	return r.Owner + "/" + r.Repo + "#" + strconv.Itoa(r.Number)
}

// API is the slice of GitHub's REST API this connector uses. Four calls: the two
// that keep one comment per run, and the two native verbs the agent can reach
// for.
type API interface {
	// CreateComment posts a comment on an issue or pull request and returns its
	// id, which is the handle for editing it.
	CreateComment(ctx context.Context, ref Ref, body string) (id int64, err error)
	// UpdateComment replaces the body of a comment.
	UpdateComment(ctx context.Context, ref Ref, id int64, body string) error
	// RequestReview asks users or teams to review a pull request.
	RequestReview(ctx context.Context, ref Ref, users, teams []string) error
	// AddLabels adds labels to an issue or pull request, leaving existing ones.
	AddLabels(ctx context.Context, ref Ref, labels []string) error
}

// GitHubBaseURL is the REST API root. GitHub Enterprise Server serves the same
// API under /api/v3 on its own host.
const GitHubBaseURL = "https://api.github.com"

// HTTPAPI is the real API, over net/http.
type HTTPAPI struct {
	client *httpjson.Client
	// full asks for bodies as GitHub renders them as well as their
	// Markdown (application/vnd.github.full+json): body_html is where a
	// private repository's files have signed URLs.
	full *httpjson.Client
}

// NewHTTPAPI returns an API bound to a token: a fine-grained personal access
// token, or the installation token of a GitHub App.
//
// The API version header is pinned. GitHub dates its breaking changes, and a
// request without the header gets whatever the current default is, which turns a
// GitHub release into our outage.
func NewHTTPAPI(token string, httpClient *http.Client) *HTTPAPI {
	return NewHTTPAPIAt(GitHubBaseURL, token, httpClient)
}

// NewHTTPAPIAt is NewHTTPAPI against another base URL, for GitHub Enterprise
// Server.
func NewHTTPAPIAt(base, token string, httpClient *http.Client) *HTTPAPI {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	header.Set("Accept", "application/vnd.github+json")
	header.Set("X-GitHub-Api-Version", "2022-11-28")
	full := header.Clone()
	full.Set("Accept", "application/vnd.github.full+json")
	return &HTTPAPI{
		client: &httpjson.Client{HTTP: httpClient, Base: base, Header: header},
		full:   &httpjson.Client{HTTP: httpClient, Base: base, Header: full},
	}
}

var (
	_ API = (*HTTPAPI)(nil)
	_ API = (*Fake)(nil)
)

// comment is the part of a comment response this connector reads.
type comment struct {
	ID      int64  `json:"id"`
	HTMLURL string `json:"html_url"`
}

// CreateComment implements API.
func (a *HTTPAPI) CreateComment(ctx context.Context, ref Ref, body string) (int64, error) {
	if ref.Number == 0 {
		return 0, fmt.Errorf("github: a comment needs an issue or pull request number")
	}
	var out comment
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments", ref.Owner, ref.Repo, ref.Number)
	if err := a.client.Do(ctx, http.MethodPost, path, map[string]string{"body": body}, &out); err != nil {
		return 0, fmt.Errorf("github: comment on %s: %w", ref, err)
	}
	if out.ID == 0 {
		return 0, fmt.Errorf("github: comment on %s returned no id, so it cannot be edited", ref)
	}
	return out.ID, nil
}

// UpdateComment implements API.
func (a *HTTPAPI) UpdateComment(ctx context.Context, ref Ref, id int64, body string) error {
	path := fmt.Sprintf("/repos/%s/%s/issues/comments/%d", ref.Owner, ref.Repo, id)
	if err := a.client.Do(ctx, http.MethodPatch, path, map[string]string{"body": body}, nil); err != nil {
		return fmt.Errorf("github: edit comment %d on %s: %w", id, ref, err)
	}
	return nil
}

// RequestReview implements API.
func (a *HTTPAPI) RequestReview(ctx context.Context, ref Ref, users, teams []string) error {
	if ref.Number == 0 {
		return fmt.Errorf("github: a review request needs a pull request number")
	}
	req := map[string][]string{}
	if len(users) > 0 {
		req["reviewers"] = users
	}
	if len(teams) > 0 {
		req["team_reviewers"] = teams
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/requested_reviewers", ref.Owner, ref.Repo, ref.Number)
	if err := a.client.Do(ctx, http.MethodPost, path, req, nil); err != nil {
		return fmt.Errorf("github: request review on %s: %w", ref, err)
	}
	return nil
}

// AddLabels implements API.
func (a *HTTPAPI) AddLabels(ctx context.Context, ref Ref, labels []string) error {
	if ref.Number == 0 {
		return fmt.Errorf("github: labels need an issue or pull request number")
	}
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/labels", ref.Owner, ref.Repo, ref.Number)
	if err := a.client.Do(ctx, http.MethodPost, path, map[string][]string{"labels": labels}, nil); err != nil {
		return fmt.Errorf("github: label %s: %w", ref, err)
	}
	return nil
}
