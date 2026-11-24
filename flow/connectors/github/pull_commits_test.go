package github_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/urmzd/saige/flow/connectors/github"
)

// commitsServer lists n commits of acme/repo#7 in pages of 100. Every third
// commit has no linked author account, and every fifth no linked committer.
func commitsServer(t *testing.T, n int) (*httptest.Server, *int) {
	t.Helper()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/repos/acme/repo/pulls/7/commits" || r.URL.Query().Get("per_page") != "100" {
			t.Errorf("unexpected request %s", r.URL)
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		start := min((page-1)*100, n)
		end := min(start+100, n)
		rows := []map[string]any{}
		for i := start; i < end; i++ {
			row := map[string]any{"sha": fmt.Sprintf("%040x", i+1), "author": nil, "committer": nil}
			if i%3 != 0 {
				row["author"] = map[string]any{"login": "ada", "id": 11}
			}
			if i%5 != 0 {
				row["committer"] = map[string]any{"login": "web-flow", "id": 19864447}
			}
			rows = append(rows, row)
		}
		_ = json.NewEncoder(w).Encode(rows)
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

func TestPullRequestCommitsReadEveryPageAndUnlinkedAccounts(t *testing.T) {
	server, requests := commitsServer(t, 130)
	api := github.NewHTTPAPIAt(server.URL, "token", server.Client())
	got, err := api.PullRequestCommits(context.Background(), "acme", "repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 130 || *requests != 2 {
		t.Fatalf("read %d commits in %d requests, want 130 in 2", len(got), *requests)
	}
	if c := got[0]; c.AuthorID != 0 || c.AuthorLogin != "" || c.CommitterID != 0 || c.CommitterLogin != "" {
		t.Errorf("commit without linked accounts read as %+v", c)
	}
	if c := got[1]; c.AuthorID != 11 || c.AuthorLogin != "ada" || c.CommitterID != 19864447 || c.CommitterLogin != "web-flow" {
		t.Errorf("commit with linked accounts read as %+v", c)
	}
}

func TestPullRequestCommitsFailAtGitHubsListLimit(t *testing.T) {
	for _, tc := range []struct {
		n  int
		ok bool
	}{{0, true}, {100, true}, {249, true}, {250, false}} {
		server, _ := commitsServer(t, tc.n)
		got, err := github.NewHTTPAPIAt(server.URL, "token", server.Client()).PullRequestCommits(context.Background(), "acme", "repo", 7)
		if (err == nil) != tc.ok || tc.ok && len(got) != tc.n {
			t.Errorf("%d commits: read %d, error %v", tc.n, len(got), err)
		}

		fake := github.NewFake()
		ref := github.Ref{Owner: "acme", Repo: "repo", Number: 7}
		seed := make([]github.PRCommit, tc.n)
		if err := fake.SeedPullRequestCommits(ref, seed); err != nil {
			t.Fatal(err)
		}
		got, err = fake.PullRequestCommits(context.Background(), "acme", "repo", 7)
		if (err == nil) != tc.ok || tc.ok && len(got) != tc.n {
			t.Errorf("fake, %d commits: read %d, error %v", tc.n, len(got), err)
		}
	}
}

func TestFakePullRequestCommitsKeepUnlinkedAccounts(t *testing.T) {
	fake := github.NewFake()
	ref := github.Ref{Owner: "acme", Repo: "repo", Number: 7}
	seed := []github.PRCommit{{SHA: commitSHA, AuthorID: 11, AuthorLogin: "ada"}, {SHA: commitSHA}}
	if err := fake.SeedPullRequestCommits(ref, seed); err != nil {
		t.Fatal(err)
	}
	got, err := fake.PullRequestCommits(context.Background(), "Acme", "Repo", 7)
	if err != nil || len(got) != 2 || got[0].AuthorID != 11 || got[1].AuthorID != 0 || got[1].CommitterID != 0 {
		t.Fatalf("fake commits %+v, %v", got, err)
	}
	if other, _ := fake.PullRequestCommits(context.Background(), "acme", "repo", 8); len(other) != 0 {
		t.Fatalf("another pull request reads %+v", other)
	}
}
