package github

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connector"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func fixFixture() FixPullRequest {
	return FixPullRequest{Base: RepositoryBase{Name: "main", SHA: strings.Repeat("a", 40)}, Title: "Fix retry handling", Body: "Tested with unit tests.", SourceKind: "issues", Changes: ChangeRequest{Message: "fix: retry handling", Files: []FileEdit{{Path: "a.go", Content: "fixed"}}}}
}

func TestFixPullBacklinksComeOnlyFromTrustedHostContext(t *testing.T) {
	api := NewFake()
	ref := Ref{Owner: "acme", Repo: "repo", Number: 7}
	api.SeedDefaultBranch(ref, fixFixture().Base)
	links := FixPullContext{SessionURL: "https://console.example.test/sessions/s_source", AlertURL: "https://console.example.test/alerts?alert=a_source"}
	c, err := New(Config{API: api, FixPullRequests: true, AuthorizeFixPull: func(context.Context, Ref) error { return nil }, FixPullContext: func(context.Context, Ref) (FixPullContext, error) { return links, nil }})
	if err != nil {
		t.Fatal(err)
	}
	input := map[string]any{"base": fixFixture().Base, "title": "Fix", "body": "Checked", "changes": fixFixture().Changes}
	target := address.MustParse("github://acme/repo/issues/7")
	input["context"] = map[string]string{"alert_url": "https://attacker.invalid"}
	raw, _ := json.Marshal(input)
	if _, err := c.createFixPull(connector.WithRunID(t.Context(), "run"), target, raw); err == nil {
		t.Fatal("model supplied backlink accepted")
	}
	delete(input, "context")
	raw, _ = json.Marshal(input)
	if _, err := c.createFixPull(connector.WithRunID(t.Context(), "run"), target, raw); err != nil {
		t.Fatal(err)
	}
	body := api.FixPulls()[0].Body
	if !strings.Contains(body, links.SessionURL) || !strings.Contains(body, links.AlertURL) || !strings.Contains(body, "https://github.com/acme/repo/issues/7") {
		t.Fatal(body)
	}
	links.AlertURL = "javascript:alert(1)"
	if _, err := c.createFixPull(connector.WithRunID(t.Context(), "other"), target, raw); err == nil {
		t.Fatal("invalid host link accepted")
	}
}
func TestFixPullCannotWriteWorkflowsOrAdvanceDefaultAndRetriesOnePR(t *testing.T) {
	r := fixFixture()
	api := NewFake()
	ref := Ref{Owner: "acme", Repo: "repo", Number: 7}
	api.SeedDefaultBranch(ref, r.Base)
	result, err := api.CreateFixPull(t.Context(), ref, "run", r)
	if err != nil {
		t.Fatal(err)
	}
	again, err := api.CreateFixPull(t.Context(), ref, "run", r)
	if err != nil || again.Number != result.Number || !again.Recovered {
		t.Fatal(again, err)
	}
	base, _ := api.ReadDefaultBranch(t.Context(), ref)
	if base != r.Base {
		t.Fatal("default branch moved")
	}
	got := api.FixPulls()
	if len(got) != 1 || !strings.Contains(got[0].Body, "Fixes https://github.com/acme/repo/issues/7") || !strings.HasSuffix(fixTitle(got[0].Request), "[humagen]") {
		t.Fatal(got)
	}
	r.Changes.Files[0].Path = ".github/workflows/build.yml"
	if _, err = api.CreateFixPull(t.Context(), ref, "other", r); err == nil {
		t.Fatal("workflow change accepted")
	}
	r = fixFixture()
	r.Base.SHA = strings.Repeat("b", 40)
	if _, err = api.CreateFixPull(t.Context(), ref, "other", r); err == nil {
		t.Fatal("stale base accepted")
	}
	c, err := New(Config{API: api, FixPullRequests: true, AuthorizeFixPull: func(context.Context, Ref) error { return fmt.Errorf("revoked") }})
	if err != nil {
		t.Fatal(err)
	}
	input := map[string]any{"base": fixFixture().Base, "title": "Fix", "body": "Description", "changes": fixFixture().Changes}
	raw, _ := json.Marshal(input)
	if _, err = c.createFixPull(connector.WithRunID(t.Context(), "run"), address.MustParse("github://acme/repo/issues/7"), raw); err == nil {
		t.Fatal("host denial ignored")
	}
	if len(api.FixPulls()) != 1 {
		t.Fatal("denied call created a PR")
	}
}
func TestFixPullHTTPRecoversLostCommitResponseWithoutMainWrite(t *testing.T) {
	r := fixFixture()
	ref := Ref{Owner: "acme", Repo: "repo", Number: 7}
	branch, marker := fixIdentity(ref, "run", r)
	sha := strings.Repeat("b", 40)
	var mu sync.Mutex
	created, committed, pull := false, false, false
	commitCalls, refCalls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		path := req.URL.Path
		switch {
		case req.Method == "GET" && path == "/repos/acme/repo/issues/7":
			fmt.Fprint(w, `{"number":7}`)
		case req.Method == "GET" && path == "/repos/acme/repo/pulls":
			if !pull {
				fmt.Fprint(w, "[]")
				return
			}
			_ = json.NewEncoder(w).Encode([]any{map[string]any{"number": 9, "html_url": "https://github.com/acme/repo/pull/9", "body": marker, "head": map[string]any{"ref": branch, "sha": sha, "repo": map[string]string{"full_name": "acme/repo"}}, "base": map[string]string{"ref": "main"}}})
		case req.Method == "GET" && path == "/repos/acme/repo/git/ref/heads/"+branch:
			if !created {
				http.Error(w, `{"message":"Not Found"}`, 404)
				return
			}
			head := r.Base.SHA
			if committed {
				head = sha
			}
			fmt.Fprintf(w, `{"object":{"sha":%q}}`, head)
		case req.Method == "GET" && path == "/repos/acme/repo":
			fmt.Fprint(w, `{"default_branch":"main"}`)
		case req.Method == "GET" && path == "/repos/acme/repo/git/ref/heads/main":
			fmt.Fprintf(w, `{"object":{"sha":%q}}`, r.Base.SHA)
		case req.Method == "POST" && path == "/repos/acme/repo/git/refs":
			var body map[string]string
			_ = json.NewDecoder(req.Body).Decode(&body)
			if body["ref"] != "refs/heads/"+branch || body["sha"] != r.Base.SHA {
				t.Error(body)
			}
			created = true
			refCalls++
			fmt.Fprint(w, `{}`)
		case req.Method == "POST" && path == "/graphql":
			var body struct {
				Variables struct {
					Input struct {
						Branch struct {
							BranchName string `json:"branchName"`
						} `json:"branch"`
						Expected string `json:"expectedHeadOid"`
					} `json:"input"`
				} `json:"variables"`
			}
			_ = json.NewDecoder(req.Body).Decode(&body)
			if body.Variables.Input.Branch.BranchName != branch || body.Variables.Input.Expected != r.Base.SHA {
				t.Error("wrong mutation branch", body)
			}
			committed = true
			commitCalls++
			http.Error(w, `{"message":"lost response"}`, 500)
		case req.Method == "GET" && path == "/repos/acme/repo/commits/"+sha:
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": sha, "commit": map[string]string{"message": r.Changes.Message + "\n\n" + marker}, "parents": []any{map[string]string{"sha": r.Base.SHA}}})
		case req.Method == "POST" && path == "/repos/acme/repo/pulls":
			var body struct {
				Title, Body, Head, Base string
				Draft                   bool
			}
			_ = json.NewDecoder(req.Body).Decode(&body)
			if body.Head != branch || body.Base != "main" || !body.Draft || !strings.HasSuffix(body.Title, " [humagen]") || !strings.Contains(body.Body, "Fixes https://github.com/acme/repo/issues/7") {
				t.Error(body)
			}
			pull = true
			fmt.Fprint(w, `{"number":9,"html_url":"https://github.com/acme/repo/pull/9"}`)
		default:
			t.Errorf("unexpected request %s %s", req.Method, path)
			http.Error(w, "unexpected", 400)
		}
	}))
	defer server.Close()
	api := NewHTTPAPIAt(server.URL, "token", server.Client())
	if _, err := api.CreateFixPull(t.Context(), ref, "run", r); err == nil {
		t.Fatal("lost commit response not reported")
	}
	result, err := api.CreateFixPull(t.Context(), ref, "run", r)
	if err != nil || result.Number != 9 {
		t.Fatal(result, err)
	}
	result, err = api.CreateFixPull(t.Context(), ref, "run", r)
	if err != nil || !result.Recovered {
		t.Fatal(result, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if commitCalls != 1 || refCalls != 1 {
		t.Fatal("duplicate mutation", commitCalls, refCalls)
	}
}
