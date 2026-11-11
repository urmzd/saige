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

func TestReviewMentionsReplyToTheRootAndEditOneAnswer(t *testing.T) {
	for _, replyTo := range []int64{0, 91} {
		t.Run(fmt.Sprint(replyTo), func(t *testing.T) {
			fake := github.NewFake()
			root := int64(95)
			if replyTo != 0 {
				root = replyTo
			}
			ref := github.Ref{Owner: "acme", Repo: "repo", Number: 42}
			if err := fake.SeedContext(ref, github.Context{ReviewComments: []github.ContextComment{{ID: root, Body: "Original finding"}}}); err != nil {
				t.Fatal(err)
			}
			c, tags := start(t, github.Config{API: fake, Agents: mention.NewNames("reviewer"), Interval: -1})
			raw, _ := json.Marshal(map[string]any{"action": "created", "repository": map[string]any{"name": "repo", "owner": map[string]string{"login": "acme"}}, "sender": map[string]string{"type": "User"}, "pull_request": map[string]any{"number": 42, "head": map[string]string{"sha": commitSHA}}, "comment": map[string]any{"id": 95, "in_reply_to_id": replyTo, "body": "@reviewer explain this"}})
			w := httptest.NewRecorder()
			c.ServeHTTP(w, deliver(t, "pull_request_review_comment", "delivery", string(raw)))
			if w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			var tag event.Envelope
			select {
			case tag = <-tags:
			default:
				t.Fatal("missing event")
			}
			source := address.MustParse(tag.Channel)
			if source.Params["review_comment"] != fmt.Sprint(root) {
				t.Fatalf("channel %q", tag.Channel)
			}
			for _, e := range []event.Envelope{ev(1, event.TypeStarted, `{}`), textEvent(2, "Explanation"), ev(3, event.TypeCompleted, `{"text":"Explanation"}`)} {
				for range 2 {
					if err := c.Deliver(context.Background(), source, e); err != nil {
						t.Fatal(err)
					}
				}
			}
			comments := fake.Comments()
			if len(comments) != 1 || comments[0].ReviewRoot != root || !strings.Contains(comments[0].Body, "Explanation") || comments[0].Edits < 1 {
				t.Fatalf("comments %+v", comments)
			}
			evidence, err := fake.ReadContext(context.Background(), ref, 1)
			if err != nil || len(evidence.Comments) != 0 || len(evidence.ReviewComments) != 2 {
				t.Fatalf("context %+v: %v", evidence, err)
			}
		})
	}
}

func TestReviewRepliesUseNativeEndpoints(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		fmt.Fprint(w, `{"id":123}`)
	}))
	defer server.Close()
	api := github.NewHTTPAPIAt(server.URL, "token", server.Client())
	ref := github.Ref{Owner: "acme", Repo: "repo", Number: 42}
	id, err := api.CreateReviewReply(context.Background(), ref, 91, "answer")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.UpdateReviewComment(context.Background(), ref, id, "final answer"); err != nil {
		t.Fatal(err)
	}
	want := "POST /repos/acme/repo/pulls/42/comments/91/replies,PATCH /repos/acme/repo/pulls/comments/123"
	if strings.Join(requests, ",") != want {
		t.Fatalf("requests %v", requests)
	}
}

func TestInvalidReviewTargetsNeverFallBackToTopLevelComments(t *testing.T) {
	fake := github.NewFake()
	c, _ := github.New(github.Config{API: fake})
	for _, uri := range []string{"github://acme/repo/issues/1?review_comment=91", "github://acme/repo/pull/1?review_comment=-1", "github://acme/repo/pull/1?review_comment=bad", "github://acme/repo/pull/1?review_comment="} {
		if err := c.Deliver(context.Background(), address.MustParse(uri), ev(1, event.TypeStarted, `{}`)); err == nil {
			t.Fatalf("accepted %s", uri)
		}
	}
	if len(fake.Comments()) != 0 {
		t.Fatal("unexpected top-level comment")
	}
}

func TestPlainReviewRepliesAuthorizeTheirExactThread(t *testing.T) {
	for _, grant := range []string{"github://acme/repo/pull/42?review_comment=5", "github://acme/repo/pull/42"} {
		t.Run(grant, func(t *testing.T) {
			var checked []string
			c, tags := start(t, github.Config{Agents: mention.NewNames("reviewer"), ContinueThread: func(a address.Address) bool {
				checked = append(checked, a.String())
				return a.String() == grant
			}})
			for _, tc := range []struct {
				root     int64
				text     string
				accepted bool
			}{
				{5, "Explain this", strings.Contains(grant, "review_comment=5")},
				{6, "Explain this", false},
				{6, "@reviewer explain this", true},
				{-1, "@reviewer explain this", false},
			} {
				raw, _ := json.Marshal(map[string]any{"action": "created", "repository": map[string]any{"name": "repo", "owner": map[string]string{"login": "acme"}}, "sender": map[string]string{"type": "User", "login": "ada"}, "pull_request": map[string]any{"number": 42}, "comment": map[string]any{"id": 10, "in_reply_to_id": tc.root, "body": tc.text}})
				w := httptest.NewRecorder()
				c.ServeHTTP(w, deliver(t, "pull_request_review_comment", fmt.Sprint(tc.root, tc.text), string(raw)))
				select {
				case tag := <-tags:
					if !tc.accepted || tag.Channel != fmt.Sprintf("github://acme/repo/pull/42?review_comment=%d", tc.root) {
						t.Fatalf("unexpected event: %+v", tag)
					}
				default:
					if tc.accepted {
						t.Fatalf("known reply %d dropped", tc.root)
					}
				}
			}
			if len(checked) != 2 || checked[0] != "github://acme/repo/pull/42?review_comment=5" || checked[1] != "github://acme/repo/pull/42?review_comment=6" {
				t.Fatalf("authorized wrong targets: %v", checked)
			}
		})
	}
}
