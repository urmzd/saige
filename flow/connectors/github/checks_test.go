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
	"github.com/urmzd/saige/flow/connectors/mention"
	"github.com/urmzd/saige/flow/event"
)

func TestChecksStreamAndKeepTheirPinnedCommit(t *testing.T) {
	fake := github.NewFake()
	c, err := github.New(github.Config{API: fake, Checks: true, Interval: -1})
	if err != nil {
		t.Fatal(err)
	}
	target := address.MustParse("github://acme/repo/pull/42").WithParam("head_sha", commitSHA)
	events := []event.Envelope{ev(1, event.TypeAccepted, `{}`), ev(2, event.TypeStarted, `{}`), ev(3, event.TypeToolCall, `{"id":"t1","name":"github_read_context"}`), ev(4, event.TypeCompleted, `{"text":"Review complete. Tests not run."}`)}
	for _, e := range events {
		for range 2 {
			if err := c.Deliver(context.Background(), target, e); err != nil {
				t.Fatal(err)
			}
		}
	}
	checks := fake.Checks()
	if len(checks) != 1 || checks[0].Check.HeadSHA != commitSHA || checks[0].Check.Status != "completed" || checks[0].Check.Conclusion != "neutral" || checks[0].Edits != 3 {
		t.Fatalf("checks %+v", checks)
	}
	if len(fake.Comments()) != 1 {
		t.Fatal("duplicate comments")
	}
	// A restarted sink finds the check rather than creating another check run.
	recovered, _ := github.New(github.Config{API: fake, Checks: true, Interval: -1})
	if err := recovered.Deliver(context.Background(), target, events[3]); err != nil {
		t.Fatal(err)
	}
	if len(fake.Checks()) != 1 {
		t.Fatal("duplicate check after recovery")
	}
}

func TestCheckRecoveryUsesExternalIDAndNeverPatchesTheHead(t *testing.T) {
	posts, patches := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			fmt.Fprintf(w, `{"check_runs":[{"id":17,"external_id":"run-1","name":"fiber/reviewer","head_sha":%q}]}`, commitSHA)
		case "PATCH":
			patches++
			var patch map[string]any
			_ = json.NewDecoder(r.Body).Decode(&patch)
			if _, ok := patch["head_sha"]; ok {
				t.Error("patch moved check head")
			}
			fmt.Fprint(w, `{}`)
		case "POST":
			posts++
			fmt.Fprint(w, `{"id":18}`)
		}
	}))
	defer server.Close()
	api := github.NewHTTPAPIAt(server.URL, "token", server.Client())
	id, err := api.EnsureCheck(context.Background(), github.Ref{Owner: "acme", Repo: "repo", Number: 1}, github.Check{Name: "fiber/reviewer", ExternalID: "run-1", HeadSHA: commitSHA, Status: "in_progress"})
	if err != nil || id != 17 || posts != 0 || patches != 1 {
		t.Fatalf("id=%d posts=%d patches=%d err=%v", id, posts, patches, err)
	}
}

func TestPRCommentPinsTheHeadBeforeAcceptance(t *testing.T) {
	fake := github.NewFake()
	evidence := github.Context{Subject: github.ContextSubject{Number: 42}}
	evidence.Subject.Head.SHA = commitSHA
	if err := fake.SeedContext(github.Ref{Owner: "urmzd", Repo: "fiber", Number: 42}, evidence); err != nil {
		t.Fatal(err)
	}
	c, tags := start(t, github.Config{API: fake, Checks: true, Agents: mention.NewNames("docs-bot")})
	raw := strings.Replace(issueComment, `"number": 42,`, `"number": 42, "pull_request":{"url":"https://api.github.com/repos/urmzd/fiber/pulls/42"},`, 1)
	w := httptest.NewRecorder()
	c.ServeHTTP(w, deliver(t, "issue_comment", "pin-comment", raw))
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	select {
	case e := <-tags:
		if !strings.HasSuffix(e.Channel, "?head_sha="+commitSHA) || ref(t, e).HeadSHA != commitSHA {
			t.Fatalf("unpinned event %+v", e)
		}
	default:
		t.Fatal("missing event")
	}
}

func TestPinnedReviewCannotReadEvidenceFromANewerCommit(t *testing.T) {
	fake := github.NewFake()
	evidence := github.Context{Subject: github.ContextSubject{Number: 42}}
	evidence.Subject.Head.SHA = strings.Repeat("b", 40)
	if err := fake.SeedContext(github.Ref{Owner: "acme", Repo: "repo", Number: 42}, evidence); err != nil {
		t.Fatal(err)
	}
	c, _ := github.New(github.Config{API: fake})
	target := address.MustParse("github://acme/repo/pull/42").WithParam("head_sha", commitSHA)
	_, err := c.Actions()[0].Invoke(context.Background(), target, json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "PR head changed") {
		t.Fatalf("expected stale evidence error, got %v", err)
	}
}

func TestIssuesDoNotCreateCommitChecks(t *testing.T) {
	fake := github.NewFake()
	c, _ := github.New(github.Config{API: fake, Checks: true})
	if err := c.Deliver(context.Background(), address.MustParse("github://acme/repo/issues/1"), ev(1, event.TypeStarted, `{}`)); err != nil {
		t.Fatal(err)
	}
	if len(fake.Checks()) != 0 || len(fake.Comments()) != 1 {
		t.Fatal("incorrect issue delivery")
	}
}

func TestCompletedChecksUseApplicationConclusionOrDefaultToNeutral(t *testing.T) {
	for _, tc := range []struct {
		name, text, outcome, want string
	}{
		{"ordinary text", "Tests passed", "", "neutral"},
		{"structured text", `{"format":"fiber.github.review.v1","findings":[{"severity":"error"}]}`, "", "neutral"},
		{"application success", "Application verified the result", "success", "success"},
		{"application failure", "Application found a failure", "failure", "failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := github.NewFake()
			calls := 0
			cfg := github.Config{API: fake, Checks: true, Interval: -1}
			if tc.outcome != "" {
				cfg.Conclusion = func(v github.View) string {
					calls++
					if v.Status != github.StatusDone || v.Text != tc.text {
						t.Errorf("unexpected conclusion view: %+v", v)
					}
					return tc.outcome
				}
			}
			c, err := github.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			target := address.MustParse("github://acme/repo/pull/42").WithParam("head_sha", commitSHA)
			if err := c.Deliver(context.Background(), target, ev(1, event.TypeStarted, `{}`)); err != nil {
				t.Fatal(err)
			}
			if calls != 0 {
				t.Fatal("conclusion callback ran before completion")
			}
			payload, _ := json.Marshal(map[string]string{"text": tc.text})
			if err := c.Deliver(context.Background(), target, ev(2, event.TypeCompleted, string(payload))); err != nil {
				t.Fatal(err)
			}
			checks := fake.Checks()
			if len(checks) != 1 || checks[0].Check.Conclusion != tc.want || checks[0].Check.Status != "completed" {
				t.Fatalf("checks = %+v; want completed/%s", checks, tc.want)
			}
			if tc.outcome != "" && calls != 1 {
				t.Fatalf("conclusion callback calls = %d; want 1", calls)
			}
		})
	}
}

// A stopped run finishes its check as cancelled: over, on purpose, and neither
// a pass nor a failure. The application's conclusion is not asked, because
// there is no finished answer for it to judge.
func TestAStoppedRunFinishesItsCheckAsCancelled(t *testing.T) {
	fake := github.NewFake()
	c, err := github.New(github.Config{API: fake, Checks: true, Interval: -1, Conclusion: func(github.View) string {
		t.Error("the conclusion callback ran for a stopped run")
		return "success"
	}})
	if err != nil {
		t.Fatal(err)
	}
	target := address.MustParse("github://acme/repo/pull/42").WithParam("head_sha", commitSHA)
	if err := c.Deliver(context.Background(), target, ev(1, event.TypeStarted, `{}`)); err != nil {
		t.Fatal(err)
	}
	if err := c.Deliver(context.Background(), target, ev(2, event.TypeStopped, `{"text":"Half a review","reason":"Stopped by a user"}`)); err != nil {
		t.Fatal(err)
	}
	checks := fake.Checks()
	if len(checks) != 1 || checks[0].Check.Status != "completed" || checks[0].Check.Conclusion != "cancelled" {
		t.Fatalf("checks = %+v; want completed/cancelled", checks)
	}
	if title := checks[0].Check.Output.Title; title != "stopped" {
		t.Errorf("check title = %q, want stopped", title)
	}
}
