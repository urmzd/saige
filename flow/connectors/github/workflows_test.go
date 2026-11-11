package github_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urmzd/saige/flow/connectors/github"
)

func TestWorkflowRerunUsesExactRunEndpointAndObservesNewerAttempts(t *testing.T) {
	posts := 0
	attempt := 1
	status := "completed"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			if r.URL.Path != "/repos/acme/repo/actions/runs/123/rerun" {
				t.Error("wrong rerun endpoint")
			}
			posts++
			attempt++
			status = "queued"
			w.WriteHeader(201)
			return
		}
		_ = json.NewEncoder(w).Encode(github.WorkflowRun{ID: 123, HeadSHA: commitSHA, Attempt: attempt, Status: status})
	}))
	defer server.Close()
	api := github.NewHTTPAPIAt(server.URL, "token", server.Client())
	ref := github.Ref{Owner: "acme", Repo: "repo", Number: 42}
	first, err := api.RerunWorkflow(context.Background(), ref, 123, commitSHA, 1)
	if err != nil || !first.Requested {
		t.Fatalf("first %+v error %v", first, err)
	}
	status = "completed"
	second, err := api.RerunWorkflow(context.Background(), ref, 123, commitSHA, 1)
	if err != nil || second.Requested || second.Attempt != 2 || posts != 1 {
		t.Fatalf("second %+v posts=%d err=%v", second, posts, err)
	}
	if _, err := api.RerunWorkflow(context.Background(), ref, 123, strings.Repeat("b", 40), 2); err == nil || posts != 1 {
		t.Fatal("reran a different commit")
	}
}

func TestWorkflowListsKeepPaginationAndCommitFilter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("head_sha") != commitSHA || r.URL.Query().Get("page") != "2" {
			t.Error("lost filter")
		}
		runs := make([]github.WorkflowRun, 30)
		for i := range runs {
			runs[i] = github.WorkflowRun{ID: int64(i + 1), HeadSHA: commitSHA}
		}
		_ = json.NewEncoder(w).Encode(github.WorkflowPage{Runs: runs})
	}))
	defer server.Close()
	api := github.NewHTTPAPIAt(server.URL, "token", server.Client())
	result, err := api.WorkflowRuns(context.Background(), github.Ref{Owner: "acme", Repo: "repo", Number: 42}, commitSHA, 2)
	if err != nil || result.NextPage != 3 || len(result.Runs) != 30 {
		t.Fatal(fmt.Sprint(result, err))
	}
}
