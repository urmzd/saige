package github_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connectors/github"
)

func TestParseReadReferenceForms(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"acme/docs", "github://acme/docs"},
		{" acme/docs ", "github://acme/docs"},
		{"github://acme/docs", "github://acme/docs"},
		{"https://github.com/acme/docs", "github://acme/docs"},
		{"https://github.com/acme/docs/", "github://acme/docs"},
		{"https://github.com/acme/docs#readme", "github://acme/docs"},
		{"github://acme/docs/pull/7", "github://acme/docs/pull/7"},
		{"github://acme/docs/issues/7", "github://acme/docs/issues/7"},
		{"https://github.com/acme/docs/pull/7", "github://acme/docs/pull/7"},
		{"https://github.com/acme/docs/issues/7#issuecomment-1", "github://acme/docs/issues/7"},
	} {
		got, err := github.ParseReadReference(tc.in)
		if err != nil || got.String() != tc.want {
			t.Errorf("%q: got %s, %v; want %s", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{
		"", "acme", "acme/", "/docs", "acme/docs/pull/7", "acme/docs#7", "acme/..",
		"github.com/acme/docs", "http://github.com/acme/docs", "https://evil.test/acme/docs",
		"https://user@github.com/acme/docs", "https://github.com/acme/docs?x=1",
		"https://github.com/acme/docs/pull", "https://github.com/acme", "github://acme/docs/pull",
	} {
		if got, err := github.ParseReadReference(in); err == nil {
			t.Errorf("%q: accepted as %s", in, got)
		}
	}
}

func seededRepository(t *testing.T) *github.Fake {
	t.Helper()
	fake := github.NewFake()
	var pulls, issues []github.RepositoryItem
	for i := 1; i <= 15; i++ {
		pulls = append(pulls, github.RepositoryItem{Number: i, Title: fmt.Sprintf("PR %d", i)})
		issues = append(issues, github.RepositoryItem{Number: 100 + i, Title: fmt.Sprintf("Issue %d", i)})
	}
	readme := strings.Repeat("A line of the README.\n", 400)
	if err := fake.SeedRepository("acme", "docs", github.RepositoryContext{Description: "The docs", DefaultBranch: "main", README: readme, PullRequests: pulls, Issues: issues}); err != nil {
		t.Fatal(err)
	}
	if err := fake.SeedContext(github.Ref{Owner: "acme", Repo: "docs", Number: 7}, github.Context{Subject: github.ContextSubject{Number: 7, Title: "Issue seven"}}); err != nil {
		t.Fatal(err)
	}
	return fake
}

func readContextAction(t *testing.T, c *github.Connector) func(address.Address, string) (string, error) {
	t.Helper()
	for _, a := range c.Actions() {
		if a.Name == "github_read_context" {
			return func(source address.Address, raw string) (string, error) {
				got, err := a.Invoke(context.Background(), source, json.RawMessage(raw))
				return got.Summary, err
			}
		}
	}
	t.Fatal("no github_read_context")
	return nil
}

func TestReadContextReadsARepositoryInEveryForm(t *testing.T) {
	fake := seededRepository(t)
	c, err := github.New(github.Config{API: fake, ReadRepositories: []string{"acme/docs"}})
	if err != nil {
		t.Fatal(err)
	}
	read := readContextAction(t, c)
	slack := address.MustParse("slack://T01/C01")
	for _, ref := range []string{"acme/docs", "github://acme/docs", "https://github.com/acme/docs", "ACME/Docs"} {
		args, _ := json.Marshal(map[string]string{"address": ref})
		summary, err := read(slack, string(args))
		if err != nil {
			t.Fatalf("%s: %v", ref, err)
		}
		var got github.RepositoryContext
		if err := json.Unmarshal([]byte(summary), &got); err != nil {
			t.Fatal(err)
		}
		if got.Description != "The docs" || got.DefaultBranch != "main" {
			t.Fatalf("%s: %+v", ref, got)
		}
		if len(got.PullRequests) != github.RepositoryItems || len(got.Issues) != github.RepositoryItems {
			t.Fatalf("%s: unbounded lists: %d PRs, %d issues", ref, len(got.PullRequests), len(got.Issues))
		}
		if len(got.README) > github.RepositoryREADMEBytes || !got.READMETruncated || !strings.HasSuffix(got.README, "\n") {
			t.Fatalf("%s: README not bounded at a line: %d bytes, truncated %v", ref, len(got.README), got.READMETruncated)
		}
		if !strings.HasPrefix(got.PullRequests[0].Address, "github://") || !strings.Contains(got.PullRequests[0].Address, "/pull/1") || !strings.Contains(got.Issues[0].Address, "/issues/101") {
			t.Fatalf("%s: item addresses %+v %+v", ref, got.PullRequests[0], got.Issues[0])
		}
	}
	// A number with a repository reads that issue.
	if summary, err := read(slack, `{"address":"acme/docs","number":7}`); err != nil || !strings.Contains(summary, "Issue seven") {
		t.Fatalf("repository and number: %s %v", summary, err)
	}
	if _, err := read(slack, `{"address":"acme/docs","page":2}`); err == nil {
		t.Fatal("a repository read took a second page")
	}
}

func TestRepositoryReadsKeepTheReadGrants(t *testing.T) {
	fake := seededRepository(t)
	for _, tc := range []struct {
		name, source, reference string
		grants, owners          []string
		want                    bool
	}{
		{"granted", "slack://T01/C01", "acme/docs", []string{"acme/docs"}, nil, true},
		{"not granted", "slack://T01/C01", "acme/docs", nil, nil, false},
		{"other granted", "slack://T01/C01", "acme/docs", []string{"acme/other"}, nil, false},
		{"lookalike", "slack://T01/C01", "acme/docs-other", []string{"acme/docs"}, nil, false},
		{"own repository", "github://acme/docs/issues/9", "acme/docs", nil, nil, true},
		{"other repository", "github://acme/other/issues/9", "github://acme/docs", nil, nil, false},
		{"owner not served", "slack://T01/C01", "acme/docs", []string{"acme/docs"}, []string{"other"}, false},
		{"number not granted", "slack://T01/C01", "acme/docs", nil, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := github.New(github.Config{API: fake, ReadRepositories: tc.grants, Owners: tc.owners})
			if err != nil {
				t.Fatal(err)
			}
			args, _ := json.Marshal(map[string]any{"address": tc.reference})
			if tc.name == "number not granted" {
				args, _ = json.Marshal(map[string]any{"address": tc.reference, "number": 7})
			}
			_, err = readContextAction(t, c)(address.MustParse(tc.source), string(args))
			if (err == nil) != tc.want {
				t.Fatalf("error %v, want success %v", err, tc.want)
			}
		})
	}
}

func TestHostBoundRepositoryReadsWithoutAnAddress(t *testing.T) {
	fake := seededRepository(t)
	c, err := github.New(github.Config{API: fake, Owners: []string{"acme"}})
	if err != nil {
		t.Fatal(err)
	}
	read := readContextAction(t, c)
	bound := address.Address{Connector: "github", Workspace: "acme", Path: []string{"docs"}}
	if summary, err := read(bound, `{}`); err != nil || !strings.Contains(summary, `"default_branch":"main"`) {
		t.Fatalf("bound repository: %s %v", summary, err)
	}
	if summary, err := read(bound, `{"number":7}`); err != nil || !strings.Contains(summary, "Issue seven") {
		t.Fatalf("bound repository issue: %s %v", summary, err)
	}
	bound.Workspace = "other"
	if _, err := read(bound, `{}`); err == nil {
		t.Fatal("read a repository whose owner the connector does not serve")
	}
}

func TestIssueActionsRefuseARepositoryWithTheFormToUse(t *testing.T) {
	c, err := github.New(github.Config{API: seededRepository(t), ReadRepositories: []string{"acme/docs"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range c.Actions() {
		if a.Name != "github_read_file" {
			continue
		}
		_, err := a.Invoke(context.Background(), address.MustParse("slack://T01/C01"), json.RawMessage(`{"address":"acme/docs","path":"README.md"}`))
		if err == nil || !strings.Contains(err.Error(), "github://acme/docs/pull/12") {
			t.Fatalf("error %v", err)
		}
	}
}

func TestHTTPRepositoryReadIsBounded(t *testing.T) {
	readme := strings.Repeat("Ünïcödé line of the README.\n", 300)
	seen := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method %s", r.Method)
		}
		seen[r.URL.Path] = r.URL.RawQuery
		switch r.URL.Path {
		case "/repos/acme/docs":
			fmt.Fprint(w, `{"full_name":"acme/docs","description":"The docs","default_branch":"main","html_url":"https://github.com/acme/docs"}`)
		case "/repos/acme/docs/readme":
			_ = json.NewEncoder(w).Encode(map[string]any{"encoding": "base64", "size": len(readme), "content": base64.StdEncoding.EncodeToString([]byte(readme))})
		case "/repos/acme/docs/pulls":
			fmt.Fprint(w, `[{"number":3,"title":"Fix","html_url":"https://github.com/acme/docs/pull/3"}]`)
		case "/repos/acme/docs/issues":
			items := make([]map[string]any, 0, 25)
			for i := 1; i <= 25; i++ {
				item := map[string]any{"number": i, "title": fmt.Sprintf("Item %d", i)}
				if i%2 == 0 {
					item["pull_request"] = map[string]string{"url": "x"}
				}
				items = append(items, item)
			}
			_ = json.NewEncoder(w).Encode(items)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	api := github.NewHTTPAPIAt(server.URL, "test", server.Client())
	got, err := api.ReadRepository(context.Background(), "acme", "docs")
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != "The docs" || got.DefaultBranch != "main" || len(got.PullRequests) != 1 || got.PullRequests[0].Address != "github://acme/docs/pull/3" {
		t.Fatalf("repository %+v", got)
	}
	if len(got.Issues) != github.RepositoryItems || got.Issues[0].Number != 1 || got.Issues[1].Number != 3 || got.Issues[0].Address != "github://acme/docs/issues/1" {
		t.Fatalf("issues %+v", got.Issues)
	}
	if len(got.README) > github.RepositoryREADMEBytes || !got.READMETruncated || !strings.HasPrefix(readme, got.README) {
		t.Fatalf("README %d bytes, truncated %v", len(got.README), got.READMETruncated)
	}
	if !strings.Contains(seen["/repos/acme/docs/pulls"], "per_page=10") || !strings.Contains(seen["/repos/acme/docs/issues"], "per_page=20") || !strings.Contains(seen["/repos/acme/docs/issues"], "state=open") {
		t.Fatalf("queries %v", seen)
	}
	if _, err := api.ReadRepository(context.Background(), "acme", ".."); err == nil {
		t.Fatal("read an invalid repository")
	}
}

func TestHTTPRepositoryWithoutREADMEReads(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/docs":
			fmt.Fprint(w, `{"default_branch":"main"}`)
		case "/repos/acme/docs/readme":
			http.NotFound(w, r)
		default:
			fmt.Fprint(w, `[]`)
		}
	}))
	defer server.Close()
	got, err := github.NewHTTPAPIAt(server.URL, "test", server.Client()).ReadRepository(context.Background(), "acme", "docs")
	if err != nil || got.README != "" || got.DefaultBranch != "main" {
		t.Fatalf("%+v %v", got, err)
	}
}
