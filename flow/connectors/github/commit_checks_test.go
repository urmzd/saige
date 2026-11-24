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

type checkApp struct {
	Slug string `json:"slug"`
}

type checkRun struct {
	Name       string   `json:"name"`
	HeadSHA    string   `json:"head_sha"`
	Status     string   `json:"status"`
	Conclusion string   `json:"conclusion,omitempty"`
	App        checkApp `json:"app"`
	ExternalID string   `json:"external_id,omitempty"`
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

func TestFakeCommitChecksKeepsRepeatedNames(t *testing.T) {
	api := github.NewFake()
	seed := []github.CommitCheck{
		{Kind: github.CheckKindRun, Name: "build", Status: "completed", Conclusion: "failure"},
		{Kind: github.CheckKindStatus, Name: "ci", Status: "completed", Conclusion: "success"},
		{Kind: github.CheckKindRun, Name: "build", Status: "completed", Conclusion: "success"},
		{Kind: github.CheckKindRun, Name: "fiber/reviewer", Status: "in_progress", App: "humagen", ExternalID: "r1"},
		{Kind: github.CheckKindStatus, Name: "fiber/reviewer", Status: "pending", App: "humagen", ExternalID: "r1"},
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
	own := github.ApprovalRequest{RunID: "r1", App: "humagen"}.OwnCheck
	if len(got) != 5 || got[3].Kind != github.CheckKindStatus || !own(got[2]) || own(got[0]) || got[4].App != "" || got[4].ExternalID != "" {
		t.Fatalf("fake checks %+v, want both builds, the own check, then the statuses without app", got)
	}
	if github.AllPassed(got, own) {
		t.Fatal("a failed build of a repeated name passed")
	}
	if other, _ := api.CommitChecks(context.Background(), "acme", "repo", strings.Repeat("c", 40)); len(other) != 0 {
		t.Fatalf("another commit reads %+v", other)
	}
	if err := api.SeedCommitCheck("acme", "repo", commitSHA, github.CommitCheck{Kind: "other", Name: "x", Status: "completed"}); err == nil {
		t.Fatal("seeded an unknown kind")
	}
}

func readChecks(t *testing.T, runs []checkRun, statuses []commitStatus) ([]github.CommitCheck, error) {
	t.Helper()
	server := checksServer(t, runs, statuses, commitSHA)
	defer server.Close()
	return github.NewHTTPAPIAt(server.URL, "token", server.Client()).CommitChecks(context.Background(), "acme", "repo", commitSHA)
}

func TestCommitChecksOnlySuccessNeutralSkippedPass(t *testing.T) {
	var runs []checkRun
	for _, c := range []string{"cancelled", "timed_out", "action_required", "stale", "startup_failure", "failure", ""} {
		runs = append(runs, checkRun{Name: "c-" + c, HeadSHA: commitSHA, Status: "completed", Conclusion: c})
	}
	for _, s := range []string{"queued", "in_progress", "waiting", "requested", "pending"} {
		runs = append(runs, checkRun{Name: "s-" + s, HeadSHA: commitSHA, Status: s})
	}
	got, err := readChecks(t, runs, []commitStatus{{Context: "x", State: "failure"}, {Context: "y", State: "error"}, {Context: "z", State: "unknown"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range got {
		if c.Passed() {
			t.Errorf("%s %s/%s passed", c.Name, c.Status, c.Conclusion)
		}
	}
}

func TestCommitChecksBounds(t *testing.T) {
	runsOf := func(n int) []checkRun {
		out := make([]checkRun, n)
		for i := range out {
			out[i] = checkRun{Name: fmt.Sprint(i), HeadSHA: commitSHA, Status: "completed", Conclusion: "success"}
		}
		return out
	}
	statusesOf := func(n int) []commitStatus {
		out := make([]commitStatus, n)
		for i := range out {
			out[i] = commitStatus{Context: fmt.Sprint(i), State: "success"}
		}
		return out
	}
	for _, tc := range []struct {
		runs, statuses int
		ok             bool
	}{{100, 0, true}, {200, 100, true}, {999, 999, true}, {1000, 0, false}, {0, 1000, false}, {1001, 0, false}} {
		got, err := readChecks(t, runsOf(tc.runs), statusesOf(tc.statuses))
		if (err == nil) != tc.ok {
			t.Errorf("runs=%d statuses=%d err=%v", tc.runs, tc.statuses, err)
		}
		if err == nil && len(got) != tc.runs+tc.statuses {
			t.Errorf("runs=%d statuses=%d read %d", tc.runs, tc.statuses, len(got))
		}
	}
}

func TestCommitChecksReadAppAndExternalID(t *testing.T) {
	got, err := readChecks(t, []checkRun{
		{Name: "build", HeadSHA: commitSHA, Status: "completed", Conclusion: "failure", App: checkApp{Slug: "actions"}},
		{Name: "build", HeadSHA: commitSHA, Status: "completed", Conclusion: "success", App: checkApp{Slug: "actions"}},
		{Name: "fiber/reviewer", HeadSHA: commitSHA, Status: "in_progress", App: checkApp{Slug: "humagen"}, ExternalID: "r1"},
		{Name: "fiber/reviewer", HeadSHA: commitSHA, Status: "completed", Conclusion: "failure", App: checkApp{Slug: "scanner"}, ExternalID: "r1"},
	}, []commitStatus{{Context: "fiber/reviewer", State: "pending"}})
	if err != nil {
		t.Fatal(err)
	}
	own := github.ApprovalRequest{RunID: "r1", App: "humagen"}.OwnCheck
	if len(got) != 5 || got[2].App != "humagen" || got[2].ExternalID != "r1" {
		t.Fatalf("read %+v, want every run with its app and external ID", got)
	}
	for i, want := range []bool{false, false, true, false, false} {
		if own(got[i]) != want {
			t.Errorf("check %d %+v own = %v, want %v", i, got[i], !want, want)
		}
	}
}

func TestAllPassed(t *testing.T) {
	pass := github.CommitCheck{Kind: github.CheckKindRun, Name: "ci", Status: "completed", Conclusion: "success"}
	skipped := github.CommitCheck{Kind: github.CheckKindRun, Name: "docs", Status: "completed", Conclusion: "skipped"}
	mine := github.CommitCheck{Kind: github.CheckKindRun, Name: "fiber/reviewer", Status: "in_progress", App: "humagen", ExternalID: "r1"}
	pending := github.CommitCheck{Kind: github.CheckKindStatus, Name: "slow", Status: "pending"}
	own := github.ApprovalRequest{RunID: "r1", App: "humagen"}.OwnCheck
	full := make([]github.CommitCheck, github.MaxCommitChecks)
	for i := range full {
		full[i] = pass
	}
	for _, tc := range []struct {
		name   string
		checks []github.CommitCheck
		own    func(github.CommitCheck) bool
		want   bool
	}{
		{"empty", nil, own, false},
		{"only own", []github.CommitCheck{mine}, own, false},
		{"passed and own", []github.CommitCheck{pass, skipped, mine}, own, true},
		{"own not skipped without a rule", []github.CommitCheck{pass, mine}, nil, false},
		{"one pending", []github.CommitCheck{pass, pending}, own, false},
		{"at the bound", full, own, false},
		{"below the bound", full[:github.MaxCommitChecks-1], own, true},
	} {
		if got := github.AllPassed(tc.checks, tc.own); got != tc.want {
			t.Errorf("%s: AllPassed = %v, want %v", tc.name, got, tc.want)
		}
	}
}
