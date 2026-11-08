package github_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connectors/github"
	"github.com/urmzd/saige/flow/event"
)

func TestContextReadsPRSourcesAndExposesPagination(t *testing.T) {
	seen := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen[r.URL.Path] = true
		if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer test" {
			t.Errorf("unexpected request %s", r.Method)
		}
		switch r.URL.Path {
		case "/repos/acme/repo/issues/42":
			fmt.Fprint(w, `{"number":42,"title":"PR","pull_request":{"url":"https://api.github.com/repos/acme/repo/pulls/42"}}`)
		case "/repos/acme/repo/pulls/42":
			fmt.Fprint(w, `{"head":{"sha":"abc"}}`)
		default:
			if r.URL.Query().Get("page") != "2" || r.URL.Query().Get("per_page") != "30" {
				t.Errorf("pagination %s", r.URL.RawQuery)
			}
			if strings.HasSuffix(r.URL.Path, "/files") {
				fmt.Fprint(w, `[{"filename":"main.go","patch":"+fixed","blob_url":"https://github.com/acme/repo/blob/abc/main.go"}]`)
			} else {
				comments := make([]github.ContextComment, 30)
				comments[0].Body = "Evidence"
				_ = json.NewEncoder(w).Encode(comments)
			}
		}
	}))
	defer server.Close()
	api := github.NewHTTPAPIAt(server.URL, "test", server.Client())
	got, err := api.ReadContext(context.Background(), github.Ref{Owner: "acme", Repo: "repo", Number: 42}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 6 || got.NextPage != 3 || got.Subject.Head.SHA != "abc" || got.ReviewComments[0].Body != "Evidence" || got.Files[0].Patch != "+fixed" {
		t.Fatalf("context %+v; requests %v", got, seen)
	}
}

func TestContextToolReturnsEvidenceAndConfinesRepository(t *testing.T) {
	fake := github.NewFake()
	ref := github.Ref{Owner: "acme", Repo: "repo.name", Number: 42}
	if err := fake.SeedContext(ref, github.Context{Subject: github.ContextSubject{Number: 42, Title: "Evidence title"}}); err != nil {
		t.Fatal(err)
	}
	c, err := github.New(github.Config{API: fake, Owners: []string{"acme"}})
	if err != nil {
		t.Fatal(err)
	}
	action := c.Actions()[0]
	target := address.Address{Connector: "github", Workspace: "acme", Path: []string{"repo.name", "issues", "42"}}
	got, err := action.Invoke(context.Background(), target, json.RawMessage(`{}`))
	if err != nil || !strings.Contains(got.Summary, "Evidence title") {
		t.Fatalf("result %+v: %v", got, err)
	}
	for _, raw := range []string{`{"repo":"other"}`, `{"page":-1}`, `{"number":-1}`, `{} {}`} {
		if _, err := action.Invoke(context.Background(), target, json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	for _, repo := range []string{"..", "../other", "repo?escape"} {
		target.Path[0] = repo
		if _, err := action.Invoke(context.Background(), target, json.RawMessage(`{}`)); err == nil {
			t.Fatalf("accepted repository %s", repo)
		}
	}
	target.Path[0] = "repo.name"
	target.Workspace = "other"
	if _, err := action.Invoke(context.Background(), target, json.RawMessage(`{}`)); err == nil {
		t.Fatal("accepted disallowed owner")
	}
}

func TestContextAPIErrorsRemainErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "permission denied", http.StatusForbidden) }))
	defer server.Close()
	api := github.NewHTTPAPIAt(server.URL, "test", server.Client())
	if _, err := api.ReadContext(context.Background(), github.Ref{Owner: "acme", Repo: "repo", Number: 1}, 1); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("error %v", err)
	}
}

func TestCompletedOnlyDeliveryRendersOrdinaryMarkdownOnce(t *testing.T) {
	fake := github.NewFake()
	c := sinkConnector(t, fake)
	payload, _ := json.Marshal(map[string]string{"text": "**Inspected abc.** Tests not run."})
	e := ev(4, event.TypeCompleted, string(payload))
	for range 2 {
		if err := c.Deliver(context.Background(), target, e); err != nil {
			t.Fatal(err)
		}
	}
	comments := fake.Comments()
	if len(comments) != 1 || !strings.Contains(comments[0].Body, "**Inspected abc.** Tests not run.") {
		t.Fatalf("comments %+v", comments)
	}
}
