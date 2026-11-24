package github_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/urmzd/saige/flow/connectors/github"
)

type checkRun struct {
	Name       string `json:"name"`
	HeadSHA    string `json:"head_sha"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion,omitempty"`
}

type commitStatus struct {
	Context string `json:"context"`
	State   string `json:"state"`
}

// checksServer serves runs and statuses for commitSHA in pages of 100.
func checksServer(t *testing.T, runs []checkRun, statuses []commitStatus, sha string) *httptest.Server {
	t.Helper()
	page := func(r *http.Request, n int) (int, int) {
		if r.URL.Query().Get("per_page") != "100" {
			t.Errorf("per_page = %q", r.URL.Query().Get("per_page"))
		}
		p, _ := strconv.Atoi(r.URL.Query().Get("page"))
		start := min((p-1)*100, n)
		return start, min(start+100, n)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/repo/commits/" + commitSHA + "/check-runs":
			if r.URL.Query().Get("filter") != "latest" {
				t.Error("check runs not filtered to the latest")
			}
			start, end := page(r, len(runs))
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(runs), "check_runs": runs[start:end]})
		case "/repos/acme/repo/commits/" + commitSHA + "/status":
			start, end := page(r, len(statuses))
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": sha, "total_count": len(statuses), "statuses": statuses[start:end]})
		default:
			t.Errorf("unexpected request %s", r.URL)
			http.NotFound(w, r)
		}
	}))
}

func TestCommitChecksReadsEveryRunAndStatusAcrossPages(t *testing.T) {
	var runs []checkRun
	for i := range 101 {
		runs = append(runs, checkRun{Name: fmt.Sprintf("build-%d", i), HeadSHA: commitSHA, Status: "completed", Conclusion: "success"})
	}
	runs = append(runs,
		checkRun{Name: "lint", HeadSHA: commitSHA, Status: "in_progress"},
		checkRun{Name: "test", HeadSHA: commitSHA, Status: "completed", Conclusion: "failure"},
		checkRun{Name: "docs", HeadSHA: commitSHA, Status: "completed", Conclusion: "neutral"},
		checkRun{Name: "deploy", HeadSHA: commitSHA, Status: "completed", Conclusion: "skipped"},
		checkRun{Name: "gate", HeadSHA: commitSHA, Status: "completed", Conclusion: "action_required"},
	)
	statuses := []commitStatus{{Context: "ci/legacy", State: "success"}, {Context: "ci/slow", State: "pending"}, {Context: "ci/broken", State: "error"}}
	server := checksServer(t, runs, statuses, commitSHA)
	defer server.Close()
	api := github.NewHTTPAPIAt(server.URL, "token", server.Client())
	got, err := api.CommitChecks(context.Background(), "acme", "repo", commitSHA)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(runs)+len(statuses) {
		t.Fatalf("read %d checks, want %d", len(got), len(runs)+len(statuses))
	}
	passed := map[string]bool{}
	for _, check := range got {
		passed[check.Kind+":"+check.Name] = check.Passed()
	}
	want := map[string]bool{
		"check_run:build-100": true,
		"check_run:lint":      false, // pending
		"check_run:test":      false, // failure
		"check_run:docs":      true,  // neutral
		"check_run:deploy":    true,  // skipped
		"check_run:gate":      false,
		"status:ci/legacy":    true,
		"status:ci/slow":      false, // pending
		"status:ci/broken":    false, // error
	}
	for name, ok := range want {
		if have, found := passed[name]; !found || have != ok {
			t.Errorf("%s passed = %v (found %v), want %v", name, have, found, ok)
		}
	}
	for _, check := range got {
		if check.Kind == github.CheckKindStatus && check.Name == "ci/slow" && (check.Status != "pending" || check.Conclusion != "") {
			t.Errorf("pending status read as %+v", check)
		}
	}
}

func TestCommitChecksFailClosed(t *testing.T) {
	ctx := context.Background()
	other := strings.Repeat("b", 40)
	t.Run("a run for another commit", func(t *testing.T) {
		server := checksServer(t, []checkRun{{Name: "build", HeadSHA: other, Status: "completed", Conclusion: "success"}}, nil, commitSHA)
		defer server.Close()
		if _, err := github.NewHTTPAPIAt(server.URL, "token", server.Client()).CommitChecks(ctx, "acme", "repo", commitSHA); err == nil {
			t.Fatal("read a check run of another commit")
		}
	})
	t.Run("a status for another commit", func(t *testing.T) {
		server := checksServer(t, nil, []commitStatus{{Context: "ci", State: "success"}}, other)
		defer server.Close()
		if _, err := github.NewHTTPAPIAt(server.URL, "token", server.Client()).CommitChecks(ctx, "acme", "repo", commitSHA); err == nil {
			t.Fatal("read a status of another commit")
		}
	})
	t.Run("more checks than the bound", func(t *testing.T) {
		runs := make([]checkRun, github.MaxCommitChecks+1)
		for i := range runs {
			runs[i] = checkRun{Name: fmt.Sprint(i), HeadSHA: commitSHA, Status: "completed", Conclusion: "success"}
		}
		server := checksServer(t, runs, nil, commitSHA)
		defer server.Close()
		if _, err := github.NewHTTPAPIAt(server.URL, "token", server.Client()).CommitChecks(ctx, "acme", "repo", commitSHA); err == nil {
			t.Fatal("returned a part of an unbounded check list")
		}
	})
	t.Run("a short commit or bad repository", func(t *testing.T) {
		api := github.NewHTTPAPIAt("http://127.0.0.1:1", "token", nil)
		for _, args := range [][3]string{{"acme", "repo", "abc123"}, {"acme", "../repo", commitSHA}, {"", "repo", commitSHA}} {
			if _, err := api.CommitChecks(ctx, args[0], args[1], args[2]); err == nil {
				t.Errorf("CommitChecks%v was requested", args)
			}
		}
	})
}

func TestFakeCommitChecks(t *testing.T) {
	api := github.NewFake()
	seed := []github.CommitCheck{
		{Kind: github.CheckKindRun, Name: "build", Status: "in_progress"},
		{Kind: github.CheckKindStatus, Name: "ci", Status: "completed", Conclusion: "success"},
		{Kind: github.CheckKindRun, Name: "build", Status: "completed", Conclusion: "success"},
	}
	for _, check := range seed {
		if err := api.SeedCommitCheck("acme", "repo", commitSHA, check); err != nil {
			t.Fatal(err)
		}
	}
	got, err := api.CommitChecks(context.Background(), "Acme", "Repo", commitSHA)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[0].Passed() || !got[1].Passed() || got[0].Kind != github.CheckKindRun {
		t.Fatalf("fake checks %+v, want the newer build run and the status", got)
	}
	if other, _ := api.CommitChecks(context.Background(), "acme", "repo", strings.Repeat("c", 40)); len(other) != 0 {
		t.Fatalf("another commit reads %+v", other)
	}
	if err := api.SeedCommitCheck("acme", "repo", commitSHA, github.CommitCheck{Kind: "other", Name: "x", Status: "completed"}); err == nil {
		t.Fatal("seeded an unknown kind")
	}
}
