package github

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connectors/internal/httpjson"
)

// CanWrite asks GitHub for effective repository permission, rather than trusting
// a webhook's author association or a role supplied by the model.
func (a *HTTPAPI) CanWrite(ctx context.Context, ref Ref, login string) (bool, error) {
	if err := validReadRef(ref, 1); err != nil {
		return false, err
	}
	if address.ValidWorkspace(login) != nil {
		return false, fmt.Errorf("github: invalid collaborator login")
	}
	var result struct {
		Permission string `json:"permission"`
	}
	path := fmt.Sprintf("/repos/%s/%s/collaborators/%s/permission", ref.Owner, ref.Repo, url.PathEscape(login))
	if err := a.client.Do(ctx, http.MethodGet, path, nil, &result); err != nil {
		var response *httpjson.Error
		if errors.As(err, &response) && response.Status == http.StatusNotFound {
			return false, nil
		}
		return false, fmt.Errorf("github: read collaborator permission: %w", err)
	}
	return result.Permission == "write" || result.Permission == "admin", nil
}

type mutationPull struct {
	State string `json:"state"`
	Head  struct {
		SHA  string `json:"sha"`
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
}

// CommitChanges uses GitHub's expectedHeadOid check to update a branch
// atomically. Fork PRs are refused: the source repository is the access boundary.
func (a *HTTPAPI) CommitChanges(ctx context.Context, ref Ref, expected, run string, args ChangeRequest) (CommitResult, error) {
	if err := validReadRef(ref, 1); err != nil {
		return CommitResult{}, err
	}
	if err := validateChanges(args); err != nil {
		return CommitResult{}, err
	}
	if !validSHA(expected) || run == "" {
		return CommitResult{}, fmt.Errorf("github: commit needs pinned head and request ID")
	}
	base := fmt.Sprintf("/repos/%s/%s", ref.Owner, ref.Repo)
	var pull mutationPull
	if err := a.client.Do(ctx, http.MethodGet, fmt.Sprintf("%s/pulls/%d", base, ref.Number), nil, &pull); err != nil {
		return CommitResult{}, fmt.Errorf("github: read PR for commit: %w", err)
	}
	if pull.State != "open" || pull.Head.Ref == "" || !strings.EqualFold(pull.Head.Repo.FullName, ref.Owner+"/"+ref.Repo) {
		return CommitResult{}, fmt.Errorf("github: fixes require an open PR with a branch in the source repository")
	}
	marker := mutationMarker(run, expected, args)
	if pull.Head.SHA != expected {
		// Recover a commit whose response was lost, but never apply it twice.
		if !validSHA(pull.Head.SHA) {
			return CommitResult{}, fmt.Errorf("github: invalid current PR head")
		}
		var commit struct {
			SHA    string `json:"sha"`
			URL    string `json:"html_url"`
			Commit struct {
				Message string `json:"message"`
			} `json:"commit"`
			Parents []struct {
				SHA string `json:"sha"`
			} `json:"parents"`
		}
		if err := a.client.Do(ctx, http.MethodGet, base+"/commits/"+pull.Head.SHA, nil, &commit); err != nil {
			return CommitResult{}, fmt.Errorf("github: inspect advanced head: %w", err)
		}
		if commit.SHA == pull.Head.SHA && len(commit.Parents) == 1 && commit.Parents[0].SHA == expected && strings.TrimRight(commit.Commit.Message, "\n") == args.Message+"\n\n"+marker {
			return CommitResult{SHA: commit.SHA, URL: commit.URL, Recovered: true}, nil
		}
		return CommitResult{}, fmt.Errorf("github: PR head changed; request a fix against the new commit")
	}
	additions := []map[string]string{}
	deletions := []map[string]string{}
	for _, file := range args.Files {
		if file.Delete {
			deletions = append(deletions, map[string]string{"path": file.Path})
		} else {
			additions = append(additions, map[string]string{"path": file.Path, "contents": base64.StdEncoding.EncodeToString([]byte(file.Content))})
		}
	}
	input := map[string]any{
		"branch":          map[string]string{"repositoryNameWithOwner": ref.Owner + "/" + ref.Repo, "branchName": pull.Head.Ref},
		"expectedHeadOid": expected,
		"message":         map[string]string{"headline": args.Message, "body": marker},
		"fileChanges":     map[string]any{"additions": additions, "deletions": deletions},
	}
	request := map[string]any{"query": `mutation($input: CreateCommitOnBranchInput!) { createCommitOnBranch(input:$input) { commit { oid url } } }`, "variables": map[string]any{"input": input}}
	var response struct {
		Data struct {
			Result struct {
				Commit struct {
					SHA string `json:"oid"`
					URL string `json:"url"`
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
	if err := client.Do(ctx, http.MethodPost, "/graphql", request, &response); err != nil {
		return CommitResult{}, fmt.Errorf("github: commit changes: %w", err)
	}
	if len(response.Errors) > 0 {
		return CommitResult{}, fmt.Errorf("github: commit changes: %s", response.Errors[0].Message)
	}
	result := response.Data.Result.Commit
	if !validSHA(result.SHA) || result.URL == "" {
		return CommitResult{}, fmt.Errorf("github: commit returned no SHA or URL")
	}
	return CommitResult{SHA: result.SHA, URL: result.URL}, nil
}

var _ MutationAPI = (*HTTPAPI)(nil)
