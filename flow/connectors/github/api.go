package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/urmzd/saige/flow/connectors/internal/httpjson"
)

// ErrReadRedirected reports a read by reference that GitHub answered with a
// redirect, as it does for a repository that was renamed or transferred.
// A cross-source read follows no redirect: the grants were decided for the
// repository the reference names, so only that one is read. A run's read of
// its own repository follows a redirect to another repository only when the
// cross-source rules grant that repository (FollowReadRedirect). Read a
// repository by its current name instead.
var ErrReadRedirected = errors.New("github: the repository answered from another location, which this read does not follow; use its current name")

type whereNamedKey struct{}

// ReadWhereNamed marks ctx for a read that must be answered where it was
// requested. HTTPAPI follows no redirect for a request made with it and
// fails with ErrReadRedirected instead. The connector marks every read of a
// repository other than the run's own, and LatestComments marks its own
// requests. Another API implementation that makes HTTP requests asks
// FollowReadRedirect, or checks IsReadWhereNamed, and does the same.
func ReadWhereNamed(ctx context.Context) context.Context {
	return context.WithValue(ctx, whereNamedKey{}, true)
}

// IsReadWhereNamed reports whether ctx was marked by ReadWhereNamed.
func IsReadWhereNamed(ctx context.Context) bool {
	marked, _ := ctx.Value(whereNamedKey{}).(bool)
	return marked
}

type ownReadKey struct{}

// ownRead is the mark of a read of the run's own repository: a redirect to
// another repository is followed only when grant allows it. grant is asked
// once for each repository in one read; decided keeps its answers.
type ownRead struct {
	owner, repo string
	grant       func(ctx context.Context, owner, repo string) error
	decided     *redirectDecisions
}

type redirectDecisions struct {
	mu  sync.Mutex
	err map[string]error
}

// decide is grant's answer for owner/repo, asking it the first time only.
func (o ownRead) decide(ctx context.Context, owner, repo string) error {
	key := strings.ToLower(owner + "/" + repo)
	o.decided.mu.Lock()
	defer o.decided.mu.Unlock()
	if err, ok := o.decided.err[key]; ok {
		return err
	}
	err := o.grant(ctx, owner, repo)
	if o.decided.err == nil {
		o.decided.err = map[string]error{}
	}
	o.decided.err[key] = err
	return err
}

// FollowReadRedirect decides whether a read made with ctx follows a
// redirect to owner/repo, the repository the redirect's location names.
// Both are empty when the location names no repository on the API's own
// base. It returns nil to follow, or an error that errors.Is reports as
// ErrReadRedirected:
//
//   - A context marked by ReadWhereNamed follows no redirect.
//   - A context the connector marked for a read of the run's own
//     repository follows a redirect within that repository. A redirect to
//     another repository is followed only when that repository passes the
//     cross-source rules (Config.Owners, Config.ReadRefused,
//     Config.ReadRepositories, Config.MayRead), which are asked before it is
//     requested. A location that names no repository is not followed.
//   - Any other context follows the redirect.
//
// HTTPAPI asks it for every redirect a read receives. Another API
// implementation that follows redirects asks it too.
func FollowReadRedirect(ctx context.Context, owner, repo string) error {
	if IsReadWhereNamed(ctx) {
		return ErrReadRedirected
	}
	own, ok := ctx.Value(ownReadKey{}).(ownRead)
	if !ok {
		return nil
	}
	if owner == "" || repo == "" {
		return ErrReadRedirected
	}
	if strings.EqualFold(owner, own.owner) && strings.EqualFold(repo, own.repo) {
		return nil
	}
	if own.decide(ctx, owner, repo) != nil {
		return ErrReadRedirected
	}
	return nil
}

// whereNamed is client, asking FollowReadRedirect about every redirect a
// request receives. A redirect it refuses fails the request with
// ErrReadRedirected and the response is dropped, so where it pointed
// reaches neither the caller nor an error. A redirect it allows keeps
// client's own redirect policy.
func whereNamed(base string, client *http.Client) *http.Client {
	out := &http.Client{Timeout: httpjson.DefaultTimeout}
	if client != nil {
		out = &http.Client{Transport: client.Transport, Jar: client.Jar, Timeout: client.Timeout, CheckRedirect: client.CheckRedirect}
	}
	next := out.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	root, _ := url.Parse(base)
	out.Transport = whereNamedTransport{next: next, base: root}
	return out
}

type whereNamedTransport struct {
	next http.RoundTripper
	base *url.URL
}

func (t whereNamedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	switch resp.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
	default:
		return resp, nil
	}
	var owner, repo string
	if location, err := req.URL.Parse(resp.Header.Get("Location")); err == nil {
		owner, repo = t.repository(location)
	}
	if err := FollowReadRedirect(req.Context(), owner, repo); err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	return resp, nil
}

// repository is the owner and repository a URL names under the API's own
// base (/repos/{owner}/{repo}/...), or two empty strings.
func (t whereNamedTransport) repository(u *url.URL) (owner, repo string) {
	if t.base == nil || u.Scheme != t.base.Scheme || !strings.EqualFold(u.Host, t.base.Host) {
		return "", ""
	}
	rest, ok := strings.CutPrefix(u.Path, strings.TrimSuffix(t.base.Path, "/")+"/repos/")
	if !ok {
		return "", ""
	}
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", ""
	}
	return parts[0], parts[1]
}

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
	httpClient = whereNamed(base, httpClient)
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
