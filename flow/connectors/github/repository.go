package github

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connector"
	"github.com/urmzd/saige/flow/connectors/internal/httpjson"
)

// Bounds on a repository read. A repository read is one page: it orients
// the agent, and the agent follows an item's address for the evidence.
const (
	// RepositoryItems is the most open pull requests, and the most open
	// issues, a repository read lists, most recently updated first.
	RepositoryItems = 10
	// RepositoryREADMEBytes is the most of the README a repository read
	// returns, cut at a line where one falls in the last quarter.
	RepositoryREADMEBytes = 4000
	// repositoryREADMEMax is the largest README the read decodes at all.
	repositoryREADMEMax = 1 << 20
)

// RepositoryAPI reads what a repository is: its description, its default
// branch, the head of its README and its most recently updated open pull
// requests and issues, each bounded.
type RepositoryAPI interface {
	ReadRepository(ctx context.Context, owner, repo string) (RepositoryContext, error)
}

// RepositoryContext is a bounded view of one repository.
type RepositoryContext struct {
	Repository    string `json:"repository"`
	Description   string `json:"description,omitempty"`
	DefaultBranch string `json:"default_branch"`
	URL           string `json:"html_url"`
	Archived      bool   `json:"archived,omitempty"`
	README        string `json:"readme,omitempty"`
	// READMETruncated says the README goes on past RepositoryREADMEBytes.
	READMETruncated bool             `json:"readme_truncated,omitempty"`
	PullRequests    []RepositoryItem `json:"open_pull_requests"`
	Issues          []RepositoryItem `json:"open_issues"`
}

// RepositoryItem is one open pull request or issue. Address is the
// github:// address that reads it in full.
type RepositoryItem struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	URL     string `json:"html_url"`
	Address string `json:"address"`
}

func validRepositoryName(owner, repo string) error {
	if address.ValidWorkspace(owner) != nil || !validRepository(repo) {
		return fmt.Errorf("github: repository read needs a valid owner and repository")
	}
	return nil
}

func itemAddress(owner, repo, kind string, number int) string {
	return refAddress(Ref{Owner: owner, Repo: repo, Number: number}, kind).String()
}

// ReadRepository implements RepositoryAPI. A repository without a README
// reads with none; every other failure is an error.
func (a *HTTPAPI) ReadRepository(ctx context.Context, owner, repo string) (RepositoryContext, error) {
	if err := validRepositoryName(owner, repo); err != nil {
		return RepositoryContext{}, err
	}
	base := fmt.Sprintf("/repos/%s/%s", owner, repo)
	get := func(path string, dst any) error {
		if err := a.client.Do(ctx, http.MethodGet, base+path, nil, dst); err != nil {
			return fmt.Errorf("github: read repository %s/%s: %w", owner, repo, err)
		}
		return nil
	}
	var meta struct {
		FullName      string `json:"full_name"`
		Description   string `json:"description"`
		DefaultBranch string `json:"default_branch"`
		URL           string `json:"html_url"`
		Archived      bool   `json:"archived"`
	}
	if err := get("", &meta); err != nil {
		return RepositoryContext{}, err
	}
	out := RepositoryContext{
		Repository:    owner + "/" + repo,
		Description:   meta.Description,
		DefaultBranch: meta.DefaultBranch,
		URL:           meta.URL,
		Archived:      meta.Archived,
	}
	var readme struct {
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
		Size     int    `json:"size"`
	}
	if err := get("/readme", &readme); err != nil {
		var response *httpjson.Error
		if !errors.As(err, &response) || response.Status != http.StatusNotFound {
			return RepositoryContext{}, err
		}
	} else if readme.Encoding == "base64" && readme.Size <= repositoryREADMEMax {
		if decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(readme.Content, "\n", "")); err == nil && utf8.Valid(decoded) {
			out.README, out.READMETruncated = readmeHead(string(decoded))
		}
	}
	var pulls []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		URL    string `json:"html_url"`
	}
	open := "?state=open&sort=updated&direction=desc&per_page="
	if err := get("/pulls"+open+strconv.Itoa(RepositoryItems), &pulls); err != nil {
		return RepositoryContext{}, err
	}
	out.PullRequests = make([]RepositoryItem, 0, min(len(pulls), RepositoryItems))
	for _, p := range pulls[:min(len(pulls), RepositoryItems)] {
		out.PullRequests = append(out.PullRequests, RepositoryItem{Number: p.Number, Title: p.Title, URL: p.URL, Address: itemAddress(owner, repo, "pull", p.Number)})
	}
	// The issues list includes pull requests; twice the bound leaves room
	// to drop them and still fill the list in a repository of mostly PRs.
	var issues []struct {
		Number      int    `json:"number"`
		Title       string `json:"title"`
		URL         string `json:"html_url"`
		PullRequest *struct {
			URL string `json:"url"`
		} `json:"pull_request"`
	}
	if err := get("/issues"+open+strconv.Itoa(2*RepositoryItems), &issues); err != nil {
		return RepositoryContext{}, err
	}
	out.Issues = make([]RepositoryItem, 0, RepositoryItems)
	for _, i := range issues {
		if i.PullRequest != nil || len(out.Issues) == RepositoryItems {
			continue
		}
		out.Issues = append(out.Issues, RepositoryItem{Number: i.Number, Title: i.Title, URL: i.URL, Address: itemAddress(owner, repo, "issues", i.Number)})
	}
	return out, nil
}

// readmeHead is the first RepositoryREADMEBytes of a README, on a rune
// boundary, cut back to the end of a line when one is near the bound.
func readmeHead(s string) (string, bool) {
	if len(s) <= RepositoryREADMEBytes {
		return s, false
	}
	cut := RepositoryREADMEBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	head := s[:cut]
	if nl := strings.LastIndexByte(head, '\n'); nl >= RepositoryREADMEBytes*3/4 {
		head = head[:nl+1]
	}
	return head, true
}

// SeedRepository installs a repository's view in the in-memory API. The
// value is copied, and ReadRepository applies the same bounds HTTPAPI does.
func (f *Fake) SeedRepository(owner, repo string, value RepositoryContext) error {
	if err := validRepositoryName(owner, repo); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.repositories == nil {
		f.repositories = make(map[string]RepositoryContext)
	}
	f.repositories[strings.ToLower(owner+"/"+repo)] = copyRepository(value)
	return nil
}

// ReadRepository implements RepositoryAPI.
func (f *Fake) ReadRepository(_ context.Context, owner, repo string) (RepositoryContext, error) {
	if err := validRepositoryName(owner, repo); err != nil {
		return RepositoryContext{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return RepositoryContext{}, f.Err
	}
	stored, ok := f.repositories[strings.ToLower(owner+"/"+repo)]
	if !ok {
		return RepositoryContext{}, fmt.Errorf("github: no repository %s/%s", owner, repo)
	}
	out := copyRepository(stored)
	out.Repository = owner + "/" + repo
	out.README, out.READMETruncated = readmeHead(out.README)
	out.PullRequests = out.PullRequests[:min(len(out.PullRequests), RepositoryItems)]
	out.Issues = out.Issues[:min(len(out.Issues), RepositoryItems)]
	for i := range out.PullRequests {
		out.PullRequests[i].Address = itemAddress(owner, repo, "pull", out.PullRequests[i].Number)
	}
	for i := range out.Issues {
		out.Issues[i].Address = itemAddress(owner, repo, "issues", out.Issues[i].Number)
	}
	return out, nil
}

func copyRepository(v RepositoryContext) RepositoryContext {
	v.PullRequests = append([]RepositoryItem(nil), v.PullRequests...)
	v.Issues = append([]RepositoryItem(nil), v.Issues...)
	return v
}

// readFormsHint names the reference forms a read accepts, for an error a
// model reads and corrects from.
const httpsScheme = "https"

// githubURL says whether a parsed reference is a plain https://github.com
// URL: no credentials and no query.
func githubURL(u *url.URL) bool {
	return u.Scheme == httpsScheme && u.Host == "github.com" && u.User == nil && u.RawQuery == ""
}

const readFormsHint = "use github://owner/repo/pull/12, github://owner/repo/issues/12, https://github.com/owner/repo/pull/12, owner/repo#12, or owner/repo for the repository"

// ParseReadReference reads a reference a read action was given: an issue
// or pull request (github://owner/repo/{issues|pull}/N or the github.com
// URL of one, or owner/repo#N and https://github.com/owner/repo#N, read as
// issues/N, which GitHub answers for a pull request too) or a repository
// (owner/repo, github://owner/repo or https://github.com/owner/repo, with
// or without ".git"). A repository's address has one path segment, an
// issue's or pull request's three. URLs are parsed into coordinates and
// never fetched. Nothing is checked against grants here.
func ParseReadReference(reference string) (address.Address, error) {
	reference = strings.TrimSpace(reference)
	var target address.Address
	switch {
	case strings.HasPrefix(reference, "github://"):
		a, err := address.Parse(reference)
		if err != nil {
			return address.Address{}, fmt.Errorf("%w: %s: %w", connector.ErrUndeliverable, readFormsHint, err)
		}
		target = address.Address{Connector: a.Connector, Workspace: a.Workspace, Path: a.Path, Params: a.Params}
	case strings.Contains(reference, "://"):
		u, err := url.Parse(reference)
		if err != nil || !githubURL(u) {
			return address.Address{}, fmt.Errorf("%w: expected a github.com URL; %s", connector.ErrUndeliverable, readFormsHint)
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) != 2 && len(parts) != 4 {
			return address.Address{}, fmt.Errorf("%w: expected a GitHub repository, issue or PR URL; %s", connector.ErrUndeliverable, readFormsHint)
		}
		target = address.Address{Connector: Name, Workspace: parts[0], Path: parts[1:]}
		// A repository URL with #12 names item 12, as GitHub's own links do;
		// any other fragment (#readme) leaves the repository.
		if len(parts) == 2 && itemNumber(u.Fragment) {
			target.Path = append(target.Path, "issues", u.Fragment)
		}
	default:
		owner, repo, ok := strings.Cut(reference, "/")
		repo, number, _ := strings.Cut(repo, "#")
		if !ok || strings.ContainsAny(repo, "/#?: \t\r\n") || strings.Contains(reference, "#") && !itemNumber(number) {
			return address.Address{}, fmt.Errorf("%w: %q is not a GitHub reference; %s", connector.ErrUndeliverable, reference, readFormsHint)
		}
		target = address.Address{Connector: Name, Workspace: owner, Path: []string{repo}}
		if number != "" {
			target.Path = append(target.Path, "issues", number)
		}
	}
	// A clone URL's ".git" names the same repository.
	if len(target.Path) > 0 {
		target.Path[0] = strings.TrimSuffix(target.Path[0], ".git")
	}
	if target.Workspace == "" || address.ValidWorkspace(target.Workspace) != nil || len(target.Path) == 0 || !validRepository(target.Path[0]) {
		return address.Address{}, fmt.Errorf("%w: invalid GitHub repository; %s", connector.ErrUndeliverable, readFormsHint)
	}
	if len(target.Path) != 1 && len(target.Path) != 3 {
		return address.Address{}, fmt.Errorf("%w: %s is not a repository, issue or pull request; %s", connector.ErrUndeliverable, target, readFormsHint)
	}
	return target, nil
}

// itemNumber says whether s is an issue or pull request number.
func itemNumber(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n > 0 && strconv.Itoa(n) == s
}

// isRepository says whether a github address names a whole repository.
func isRepository(target address.Address) bool {
	return target.Connector == Name && len(target.Path) == 1
}

// repositoryTarget checks a repository address the way readTarget checks
// an issue: the connector serves its owner, and it is the run's own
// repository or one granted for cross-source reads.
func (c *Connector) repositoryTarget(source, target address.Address) (owner, repo string, err error) {
	owner, repo = target.Workspace, target.Path[0]
	if c.owners != nil && !c.owners[strings.ToLower(owner)] {
		return "", "", fmt.Errorf("%w: %s is owned by %q, which this connector does not serve", connector.ErrUndeliverable, target, owner)
	}
	if address.ValidWorkspace(owner) != nil || !validRepository(repo) {
		return "", "", fmt.Errorf("%w: invalid GitHub repository", connector.ErrUndeliverable)
	}
	if source.Connector == Name && len(source.Path) > 0 && strings.EqualFold(source.Workspace, owner) && strings.EqualFold(source.Path[0], repo) {
		return owner, repo, nil
	}
	if !c.readRepos[strings.ToLower(owner+"/"+repo)] {
		return "", "", fmt.Errorf("%w: repository %s/%s is not granted for cross-source reads", connector.ErrUndeliverable, owner, repo)
	}
	return owner, repo, nil
}
