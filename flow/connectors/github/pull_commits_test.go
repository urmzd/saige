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

// commitsOf serves one page of the given commit rows for acme/repo#7.
func commitsOf(t *testing.T, rows []map[string]any) *github.HTTPAPI {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(rows)
	}))
	t.Cleanup(server.Close)
	return github.NewHTTPAPIAt(server.URL, "token", server.Client())
}

// A commit carries its git author and committer emails and the co-authors its
// message names; GitHub's web-flow committer is told apart by ID and login.
func TestPullRequestCommitsReadEmailsCoAuthorsAndWebFlow(t *testing.T) {
	message := "Merge the change\n\nBody text.\nCo-authored-by: Ada Lovelace <ada@example.com>\n  co-AUTHORED-BY:   Grace <grace@example.com>  \r\nCo-authored-by: no address here\nNot co-authored-by: x <x@example.com>\n"
	api := commitsOf(t, []map[string]any{
		{
			"sha":       fmt.Sprintf("%040x", 1),
			"commit":    map[string]any{"message": message, "author": map[string]any{"name": "Ada", "email": "ada@example.com"}, "committer": map[string]any{"name": "GitHub", "email": "noreply@github.com"}},
			"author":    map[string]any{"login": "ada", "id": 11},
			"committer": map[string]any{"login": "web-flow", "id": 19864447},
		},
		{
			"sha":       fmt.Sprintf("%040x", 2),
			"commit":    map[string]any{"message": "plain", "author": map[string]any{"email": "someone@example.com"}, "committer": map[string]any{"email": "someone@example.com"}},
			"author":    map[string]any{"login": "x", "id": -5},
			"committer": map[string]any{"login": "web-flow", "id": 1},
		},
	})
	got, err := api.PullRequestCommits(context.Background(), "acme", "repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	first := got[0]
	if first.AuthorEmail != "ada@example.com" || first.CommitterEmail != "noreply@github.com" || !first.CommittedByWebFlow() {
		t.Errorf("first commit read as %+v", first)
	}
	want := []github.CoAuthor{{Name: "Ada Lovelace", Email: "ada@example.com"}, {Name: "Grace", Email: "grace@example.com"}, {Name: "no address here"}}
	if len(first.CoAuthors) != len(want) {
		t.Fatalf("co-authors %+v, want %+v", first.CoAuthors, want)
	}
	for i := range want {
		if first.CoAuthors[i] != want[i] {
			t.Errorf("co-author %d = %+v, want %+v", i, first.CoAuthors[i], want[i])
		}
	}
	second := got[1]
	if second.AuthorID != 0 || second.AuthorEmail != "someone@example.com" || second.CommittedByWebFlow() || len(second.CoAuthors) != 0 {
		t.Errorf("second commit read as %+v, want author ID 0 and no web-flow committer", second)
	}
}

// A commit that names more than MaxCoAuthors co-authors fails the read.
func TestPullRequestCommitsBoundCoAuthors(t *testing.T) {
	for _, tc := range []struct {
		n  int
		ok bool
	}{{github.MaxCoAuthors, true}, {github.MaxCoAuthors + 1, false}} {
		message := "subject\n"
		for i := range tc.n {
			message += fmt.Sprintf("\nCo-authored-by: P%d <p%d@example.com>", i, i)
		}
		api := commitsOf(t, []map[string]any{{"sha": fmt.Sprintf("%040x", 1), "commit": map[string]any{"message": message}}})
		got, err := api.PullRequestCommits(context.Background(), "acme", "repo", 7)
		if (err == nil) != tc.ok || tc.ok && len(got[0].CoAuthors) != tc.n {
			t.Errorf("%d co-authors: read %+v, error %v", tc.n, got, err)
		}
	}
}

// A mid-list read error fails the read rather than return part of the list.
func TestPullRequestCommitsFailOnALaterPageError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"message":"bad gateway"}`))
			return
		}
		rows := make([]map[string]any, 100)
		for i := range rows {
			rows[i] = map[string]any{"sha": fmt.Sprintf("%040x", i+1)}
		}
		_ = json.NewEncoder(w).Encode(rows)
	}))
	t.Cleanup(server.Close)
	if got, err := github.NewHTTPAPIAt(server.URL, "token", server.Client()).PullRequestCommits(context.Background(), "acme", "repo", 7); err == nil {
		t.Fatalf("read %d commits despite a failed page", len(got))
	}
}

// The fake reads an ID below 1 as 0, as the HTTP API does.
func TestFakePullRequestCommitsReadIDsBelowOneAsZero(t *testing.T) {
	fake := github.NewFake()
	ref := github.Ref{Owner: "acme", Repo: "repo", Number: 7}
	if err := fake.SeedPullRequestCommits(ref, []github.PRCommit{{SHA: commitSHA, AuthorID: -2, CommitterID: -1}}); err != nil {
		t.Fatal(err)
	}
	got, err := fake.PullRequestCommits(context.Background(), "acme", "repo", 7)
	if err != nil || got[0].AuthorID != 0 || got[0].CommitterID != 0 {
		t.Fatalf("fake commits %+v, %v", got, err)
	}
}

// Co-author lines allow space before the colon, end at a lone CR, and give
// the first address in angle brackets whatever follows it.
func TestPullRequestCommitsCoAuthorForms(t *testing.T) {
	message := "subject\r\rCo-authored-by : Ada <ada@example.com>\rCo-Authored-By\t:Bob <bob@example.com> (pairing)\r\nco-authored-by:Cy <cy@example.com> <cy2@example.com>\nCo-authored-byX: Not <not@example.com>\nCo-authored-by Dee <dee@example.com>"
	api := commitsOf(t, []map[string]any{{"sha": fmt.Sprintf("%040x", 1), "commit": map[string]any{"message": message}}})
	got, err := api.PullRequestCommits(context.Background(), "acme", "repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	want := []github.CoAuthor{{Name: "Ada", Email: "ada@example.com"}, {Name: "Bob", Email: "bob@example.com"}, {Name: "Cy", Email: "cy@example.com"}}
	if len(got[0].CoAuthors) != len(want) {
		t.Fatalf("co-authors %+v, want %+v", got[0].CoAuthors, want)
	}
	for i := range want {
		if got[0].CoAuthors[i] != want[i] {
			t.Errorf("co-author %d = %+v, want %+v", i, got[0].CoAuthors[i], want[i])
		}
	}
}

// A commit carries GitHub's signature verdict.
func TestPullRequestCommitsReadVerification(t *testing.T) {
	api := commitsOf(t, []map[string]any{
		{"sha": fmt.Sprintf("%040x", 1), "commit": map[string]any{"message": "a", "verification": map[string]any{"verified": true, "reason": "valid", "signature": "sig", "payload": "p"}}},
		{"sha": fmt.Sprintf("%040x", 2), "commit": map[string]any{"message": "b", "verification": map[string]any{"verified": false, "reason": "unsigned"}}},
		{"sha": fmt.Sprintf("%040x", 3), "commit": map[string]any{"message": "c"}},
	})
	got, err := api.PullRequestCommits(context.Background(), "acme", "repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	want := []github.CommitVerification{{Verified: true, Reason: "valid"}, {Reason: "unsigned"}, {}}
	for i := range want {
		if got[i].Verification != want[i] {
			t.Errorf("commit %d verification %+v, want %+v", i, got[i].Verification, want[i])
		}
	}
}
