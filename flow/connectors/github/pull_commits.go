package github

import (
	"context"
	"fmt"
	"net/http"
)

// PullRequestCommitsAPI reads the commits of a pull request, so a host can
// refuse an approval asked for by someone who wrote one of them. The model has
// no tool for it.
type PullRequestCommitsAPI interface {
	PullRequestCommits(ctx context.Context, owner, repo string, number int) ([]PRCommit, error)
}

// PRCommit is one commit of a pull request and the GitHub accounts linked to
// its author and committer. An ID is 0, and its login empty, when GitHub links
// no account, such as a commit made with an email no account claims.
type PRCommit struct {
	SHA            string `json:"sha"`
	AuthorID       int64  `json:"author_id,omitempty"`
	CommitterID    int64  `json:"committer_id,omitempty"`
	AuthorLogin    string `json:"author_login,omitempty"`
	CommitterLogin string `json:"committer_login,omitempty"`
}

// MaxPullRequestCommits is how many commits GitHub lists for a pull request.
// A read that reaches it fails: the list may be cut short, and a host that
// checks who wrote the commits cannot tell.
const MaxPullRequestCommits = 250

const pullCommitsPage = 100

// PullRequestCommits reads every commit of a pull request, following pages of
// 100 until one comes back short. It fails when the pull request has
// MaxPullRequestCommits or more commits.
func (a *HTTPAPI) PullRequestCommits(ctx context.Context, owner, repo string, number int) ([]PRCommit, error) {
	ref := Ref{Owner: owner, Repo: repo, Number: number}
	if err := validReadRef(ref, 1); err != nil {
		return nil, err
	}
	type account struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
	}
	var out []PRCommit
	for page := 1; ; page++ {
		var rows []struct {
			SHA       string   `json:"sha"`
			Author    *account `json:"author"`
			Committer *account `json:"committer"`
		}
		path := fmt.Sprintf("/repos/%s/%s/pulls/%d/commits?per_page=%d&page=%d", owner, repo, number, pullCommitsPage, page)
		if err := a.client.Do(ctx, http.MethodGet, path, nil, &rows); err != nil {
			return nil, fmt.Errorf("github: list commits of %s: %w", ref, err)
		}
		for _, row := range rows {
			if !validSHA(row.SHA) {
				return nil, fmt.Errorf("github: commit list of %s has an invalid SHA", ref)
			}
			c := PRCommit{SHA: row.SHA}
			if row.Author != nil {
				c.AuthorID, c.AuthorLogin = row.Author.ID, row.Author.Login
			}
			if row.Committer != nil {
				c.CommitterID, c.CommitterLogin = row.Committer.ID, row.Committer.Login
			}
			out = append(out, c)
		}
		if len(out) >= MaxPullRequestCommits {
			return nil, fmt.Errorf("github: %s has %d or more commits", ref, MaxPullRequestCommits)
		}
		if len(rows) < pullCommitsPage {
			return out, nil
		}
	}
}

// SeedPullRequestCommits sets the commits of a pull request in the fake.
func (f *Fake) SeedPullRequestCommits(ref Ref, commits []PRCommit) error {
	if err := validReadRef(ref, 1); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pullCommits == nil {
		f.pullCommits = make(map[string][]PRCommit)
	}
	f.pullCommits[pullCommitsKey(ref)] = append([]PRCommit(nil), commits...)
	return nil
}

// PullRequestCommits returns the seeded commits, and fails at
// MaxPullRequestCommits as the HTTP API does.
func (f *Fake) PullRequestCommits(_ context.Context, owner, repo string, number int) ([]PRCommit, error) {
	ref := Ref{Owner: owner, Repo: repo, Number: number}
	if err := validReadRef(ref, 1); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	commits := f.pullCommits[pullCommitsKey(ref)]
	if len(commits) >= MaxPullRequestCommits {
		return nil, fmt.Errorf("github: %s has %d or more commits", ref, MaxPullRequestCommits)
	}
	return append([]PRCommit(nil), commits...), nil
}

func pullCommitsKey(ref Ref) string { return workflowKey(ref) + "#" + fmt.Sprint(ref.Number) }

var (
	_ PullRequestCommitsAPI = (*HTTPAPI)(nil)
	_ PullRequestCommitsAPI = (*Fake)(nil)
)
