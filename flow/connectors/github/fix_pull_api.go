package github

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/urmzd/saige/flow/connectors/internal/httpjson"
)

func (a *HTTPAPI) ReadDefaultBranch(ctx context.Context, ref Ref) (RepositoryBase, error) {
	if err := validReadRef(ref, 1); err != nil {
		return RepositoryBase{}, err
	}
	root := fmt.Sprintf("/repos/%s/%s", ref.Owner, ref.Repo)
	var repo struct {
		DefaultBranch string `json:"default_branch"`
		Archived      bool   `json:"archived"`
		Disabled      bool   `json:"disabled"`
	}
	if err := a.client.Do(ctx, http.MethodGet, root, nil, &repo); err != nil {
		return RepositoryBase{}, err
	}
	if repo.DefaultBranch == "" || repo.Archived || repo.Disabled {
		return RepositoryBase{}, fmt.Errorf("github: repository is unavailable for fixes")
	}
	sha, err := a.fixBranchHead(ctx, root, repo.DefaultBranch)
	if err != nil {
		return RepositoryBase{}, err
	}
	return RepositoryBase{Name: repo.DefaultBranch, SHA: sha}, nil
}
func (a *HTTPAPI) fixBranchHead(ctx context.Context, root, name string) (string, error) {
	var result struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := a.client.Do(ctx, http.MethodGet, root+"/git/ref/heads/"+url.PathEscape(name), nil, &result); err != nil {
		return "", err
	}
	if !validSHA(result.Object.SHA) {
		return "", fmt.Errorf("github: branch returned invalid head")
	}
	return result.Object.SHA, nil
}
func (a *HTTPAPI) CreateFixPull(ctx context.Context, ref Ref, run string, r FixPullRequest) (FixPullResult, error) {
	if err := validReadRef(ref, 1); err != nil {
		return FixPullResult{}, err
	}
	if err := validateFixPull(r); err != nil {
		return FixPullResult{}, err
	}
	if run == "" {
		return FixPullResult{}, fmt.Errorf("github: fix needs run identity")
	}
	root := fmt.Sprintf("/repos/%s/%s", ref.Owner, ref.Repo)
	var source struct {
		Number      int       `json:"number"`
		PullRequest *struct{} `json:"pull_request"`
	}
	if err := a.client.Do(ctx, http.MethodGet, fmt.Sprintf("%s/issues/%d", root, ref.Number), nil, &source); err != nil {
		return FixPullResult{}, err
	}
	if source.Number != ref.Number || (source.PullRequest != nil) != (r.SourceKind == "pull") {
		return FixPullResult{}, fmt.Errorf("github: source issue or pull request does not match")
	}
	branch, marker := fixIdentity(ref, run, r)
	if branch == r.Base.Name {
		return FixPullResult{}, fmt.Errorf("github: fix branch cannot be the default branch")
	}
	// Recover an existing immutable PR before checking whether main advanced.
	existing, err := a.findFixPull(ctx, root, ref, branch, r.Base.Name, marker)
	if err != nil {
		return FixPullResult{}, err
	}
	if existing.Number > 0 {
		existing.Recovered = true
		return existing, nil
	}
	head, err := a.fixBranchHead(ctx, root, branch)
	if err != nil {
		var response *httpjson.Error
		if !errors.As(err, &response) || response.Status != http.StatusNotFound {
			return FixPullResult{}, err
		}
		current, err := a.ReadDefaultBranch(ctx, ref)
		if err != nil {
			return FixPullResult{}, err
		}
		if current != r.Base {
			return FixPullResult{}, fmt.Errorf("github: default branch changed; inspect the new revision before creating a fix")
		}
		err = a.client.Do(ctx, http.MethodPost, root+"/git/refs", map[string]string{"ref": "refs/heads/" + branch, "sha": r.Base.SHA}, nil)
		if err != nil { // A concurrent retry may have created the exact branch.
			head, err = a.fixBranchHead(ctx, root, branch)
			if err != nil {
				return FixPullResult{}, err
			}
		} else {
			head = r.Base.SHA
		}
	}
	if head == r.Base.SHA {
		head, err = a.commitFixBranch(ctx, ref, branch, r.Base.SHA, r.Changes, marker)
		if err != nil {
			return FixPullResult{}, err
		}
	} else {
		var commit struct {
			SHA    string `json:"sha"`
			Commit struct {
				Message string `json:"message"`
			} `json:"commit"`
			Parents []struct {
				SHA string `json:"sha"`
			} `json:"parents"`
		}
		if err = a.client.Do(ctx, http.MethodGet, root+"/commits/"+head, nil, &commit); err != nil {
			return FixPullResult{}, err
		}
		if commit.SHA != head || len(commit.Parents) != 1 || commit.Parents[0].SHA != r.Base.SHA || strings.TrimRight(commit.Commit.Message, "\n") != r.Changes.Message+"\n\n"+marker {
			return FixPullResult{}, fmt.Errorf("github: fix branch changed; refusing to overwrite it")
		}
	}
	payload := map[string]any{"title": fixTitle(r), "body": fixBody(ref, r, marker), "head": branch, "base": r.Base.Name, "draft": true}
	var result FixPullResult
	if err = a.client.Do(ctx, http.MethodPost, root+"/pulls", payload, &result); err != nil {
		recovered, recoveryErr := a.findFixPull(ctx, root, ref, branch, r.Base.Name, marker)
		if recoveryErr == nil && recovered.Number > 0 {
			recovered.Recovered = true
			return recovered, nil
		}
		return FixPullResult{}, err
	}
	if result.Number <= 0 || result.URL == "" {
		return FixPullResult{}, fmt.Errorf("github: fix PR returned no native identity")
	}
	result.Branch, result.SHA = branch, head
	return result, nil
}
func (a *HTTPAPI) findFixPull(ctx context.Context, root string, ref Ref, branch, base, marker string) (FixPullResult, error) {
	for page := 1; page <= 10; page++ {
		var rows []struct {
			Number int    `json:"number"`
			URL    string `json:"html_url"`
			Body   string `json:"body"`
			Head   struct {
				SHA  string `json:"sha"`
				Ref  string `json:"ref"`
				Repo struct {
					FullName string `json:"full_name"`
				} `json:"repo"`
			} `json:"head"`
			Base struct {
				Ref string `json:"ref"`
			} `json:"base"`
		}
		endpoint := root + "/pulls?state=all&head=" + url.QueryEscape(ref.Owner+":"+branch) + "&base=" + url.QueryEscape(base) + fmt.Sprintf("&per_page=100&page=%d", page)
		if err := a.client.Do(ctx, http.MethodGet, endpoint, nil, &rows); err != nil {
			return FixPullResult{}, err
		}
		for _, row := range rows {
			if row.Head.Ref == branch && row.Base.Ref == base && strings.EqualFold(row.Head.Repo.FullName, ref.Owner+"/"+ref.Repo) && strings.Contains(row.Body, marker) && row.Number > 0 && validSHA(row.Head.SHA) {
				return FixPullResult{Number: row.Number, URL: row.URL, Branch: branch, SHA: row.Head.SHA}, nil
			}
		}
		if len(rows) < 100 {
			return FixPullResult{}, nil
		}
	}
	return FixPullResult{}, fmt.Errorf("github: fix recovery exceeds 1000 pull requests")
}
func (a *HTTPAPI) commitFixBranch(ctx context.Context, ref Ref, branch, head string, changes ChangeRequest, marker string) (string, error) {
	additions := []map[string]string{}
	deletions := []map[string]string{}
	for _, file := range changes.Files {
		if file.Delete {
			deletions = append(deletions, map[string]string{"path": file.Path})
		} else {
			additions = append(additions, map[string]string{"path": file.Path, "contents": base64.StdEncoding.EncodeToString([]byte(file.Content))})
		}
	}
	input := map[string]any{"branch": map[string]string{"repositoryNameWithOwner": ref.Owner + "/" + ref.Repo, "branchName": branch}, "expectedHeadOid": head, "message": map[string]string{"headline": changes.Message, "body": marker}, "fileChanges": map[string]any{"additions": additions, "deletions": deletions}}
	req := map[string]any{"query": `mutation($input: CreateCommitOnBranchInput!) { createCommitOnBranch(input:$input) { commit { oid } } }`, "variables": map[string]any{"input": input}}
	var response struct {
		Data struct {
			Result struct {
				Commit struct {
					SHA string `json:"oid"`
				} `json:"commit"`
			} `json:"createCommitOnBranch"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	client := *a.client
	if strings.HasSuffix(client.Base, "/api/v3") {
		client.Base = strings.TrimSuffix(client.Base, "/v3")
	}
	if err := client.Do(ctx, http.MethodPost, "/graphql", req, &response); err != nil {
		return "", err
	}
	if len(response.Errors) > 0 || !validSHA(response.Data.Result.Commit.SHA) {
		return "", fmt.Errorf("github: fix commit refused or returned no immutable SHA")
	}
	return response.Data.Result.Commit.SHA, nil
}
