package github_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/flow/connectors/internal/rendertest"
	"github.com/urmzd/saige/flow/event"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connector"
	"github.com/urmzd/saige/flow/connectors/github"
	"github.com/urmzd/saige/flow/connectors/mention"
	"github.com/urmzd/saige/flow/signature"
)

const secret = "It's a Secret to Everybody"

// deliver builds a webhook delivery signed the way GitHub signs one. The real
// verifier from pkg/signature is used rather than a permissive double: this
// signature is the trigger's only security boundary.
func deliver(t *testing.T, event, delivery, body string) *http.Request {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))

	r := httptest.NewRequest(http.MethodPost, "/github/webhook", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(github.HeaderEvent, event)
	r.Header.Set(github.HeaderDelivery, delivery)
	r.Header.Set(signature.HeaderGitHubSignature, "sha256="+hex.EncodeToString(mac.Sum(nil)))
	return r
}

func start(t *testing.T, cfg github.Config) (*github.Connector, <-chan event.Envelope) {
	t.Helper()
	if cfg.API == nil {
		cfg.API = github.NewFake()
	}
	if cfg.Verifier == nil {
		cfg.Verifier = signature.NewGitHub(secret)
	}
	c, err := github.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tags := make(chan event.Envelope, 8)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.Ingest(ctx, func(_ context.Context, e event.Envelope) error {
			tags <- e
			return nil
		})
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	for range 1000 {
		if c.Ingesting() {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !c.Ingesting() {
		t.Fatal("Ingest never claimed the channel")
	}
	return c, tags
}

// issueComment is a realistic issue_comment delivery on an issue, with the
// mention mid-sentence and repeated.
const issueComment = `{
  "action": "created",
  "issue": {
    "number": 42,
    "title": "Retries double-post to Slack",
    "body": "Steps to reproduce...",
    "state": "open",
    "user": {"login": "urmzd", "type": "User"},
    "labels": [{"name": "bug"}, {"name": "connectors"}],
    "html_url": "https://github.com/urmzd/saige/flow/issues/42",
    "created_at": "2026-07-20T10:00:00Z",
    "updated_at": "2026-07-20T11:00:00Z"
  },
  "comment": {
    "id": 2201234567,
    "body": "can @docs-bot summarize what changed here, and @docs-bot check the retry path too?",
    "user": {"login": "urmzd", "type": "User"},
    "html_url": "https://github.com/urmzd/saige/flow/issues/42#issuecomment-2201234567",
    "created_at": "2026-07-20T12:30:00Z"
  },
  "repository": {
    "name": "fiber",
    "full_name": "urmzd/fiber",
    "private": false,
    "owner": {"login": "urmzd", "type": "User"}
  },
  "sender": {"login": "urmzd", "type": "User"},
  "installation": {"id": 55555}
}`

func TestIssueCommentPublishesAnEventOnTheIssue(t *testing.T) {
	c, tags := start(t, github.Config{Agents: mention.NewNames("docs-bot"), Tenant: "acme"})

	w := httptest.NewRecorder()
	c.ServeHTTP(w, deliver(t, "issue_comment", "d1f2e3c4-0000-1111-2222-333344445555", issueComment))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", w.Code, w.Body.String())
	}

	e := receive(t, tags)
	if e.ID != "d1f2e3c4-0000-1111-2222-333344445555" {
		t.Errorf("ID = %q, want the X-GitHub-Delivery guid", e.ID)
	}
	if e.Type != event.TypeGitHubIssueCommentCreated || e.Tenant != "acme" || e.Subject != "issues/42" {
		t.Errorf("type, tenant, subject = %q, %q, %q", e.Type, e.Tenant, e.Subject)
	}
	r := request(t, e)
	if !slices.Equal(r.Mentions, []string{"docs-bot"}) {
		t.Errorf("Mentions = %v, want docs-bot once", r.Mentions)
	}
	want := "can summarize what changed here, and check the retry path too?"
	if r.Text != want {
		t.Errorf("Text = %q, want %q with both mentions stripped", r.Text, want)
	}
	if want := "github://urmzd/fiber/issues/42"; e.Channel != want || e.Source != want {
		t.Errorf("Channel, Source = %q, %q, want %q", e.Channel, e.Source, want)
	}
	if r.Actor.ID != "urmzd" || r.Actor.Bot {
		t.Errorf("Actor = %+v", r.Actor)
	}
	if !e.Time.Equal(time.Date(2026, 7, 20, 12, 30, 0, 0, time.UTC)) {
		t.Errorf("Time = %s, want the comment's own timestamp", e.Time)
	}
	wantRef := event.GitHubRef{
		Owner: "urmzd", Repo: "fiber", Number: 42, Title: "Retries double-post to Slack", Sender: "urmzd",
		State: "open", Author: "urmzd", URL: "https://github.com/urmzd/saige/flow/issues/42",
		Labels: []string{"bug", "connectors"}, InstallationID: 55555,
	}
	if got := ref(t, e); !reflect.DeepEqual(got, wantRef) {
		t.Errorf("ref = %+v, want %+v", got, wantRef)
	}
	if b := e.Data.(event.GitHubIssueCommentCreated); b.CommentID != 2201234567 {
		t.Errorf("CommentID = %d", b.CommentID)
	}
}

func TestACommentOnAPullRequestAddressesPullNotIssues(t *testing.T) {
	// GitHub delivers a comment on a PR as an issue_comment whose issue carries
	// a pull_request member. The address must say what the agent is looking at.
	body := strings.Replace(issueComment,
		`"state": "open",`,
		`"state": "open", "pull_request": {"url": "https://api.github.com/repos/urmzd/fiber/pulls/42"},`, 1)

	c, tags := start(t, github.Config{Agents: mention.NewNames("docs-bot")})
	w := httptest.NewRecorder()
	c.ServeHTTP(w, deliver(t, "issue_comment", "d2", body))
	e := receive(t, tags)

	if got, want := e.Channel, "github://urmzd/fiber/pull/42"; got != want {
		t.Errorf("Channel = %q, want %q", got, want)
	}
	if !ref(t, e).Pull {
		t.Error("ref.Pull = false, want a pull request")
	}
}

// reviewRequested is a realistic pull_request review_requested delivery: the
// agent is named by GitHub, and there is no message at all.
const reviewRequested = `{
  "action": "review_requested",
  "number": 77,
  "pull_request": {
    "number": 77,
    "title": "Coalesce sink edits",
    "body": "Buffers deltas and flushes on an interval.\n\ncc @urmzd",
    "state": "open",
    "draft": false,
    "user": {"login": "urmzd", "type": "User"},
    "labels": [{"name": "connectors"}],
    "html_url": "https://github.com/urmzd/saige/flow/pull/77",
    "base": {"ref": "main"},
    "head": {"ref": "coalesce-edits", "sha": "deadbeef"},
    "created_at": "2026-07-21T09:00:00Z",
    "updated_at": "2026-07-21T09:30:00Z"
  },
  "requested_reviewer": {"login": "docs-bot[bot]", "type": "Bot"},
  "repository": {
    "name": "fiber",
    "full_name": "urmzd/fiber",
    "owner": {"login": "urmzd", "type": "User"}
  },
  "sender": {"login": "urmzd", "type": "User"}
}`

func TestAReviewRequestNamesTheAgentWithoutAnyMention(t *testing.T) {
	c, tags := start(t, github.Config{
		// The app's login carries the [bot] suffix in the payload; mapping the
		// bare login covers both spellings.
		Bots: map[string]string{"docs-bot": "docs-bot"},
	})

	w := httptest.NewRecorder()
	c.ServeHTTP(w, deliver(t, "pull_request", "d3", reviewRequested))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	e := receive(t, tags)
	b, ok := e.Data.(event.GitHubReviewRequested)
	if !ok {
		t.Fatalf("published %s, want %s", e.Type, event.TypeGitHubReviewRequested)
	}
	if b.Reviewer != "docs-bot" || len(b.Mentions) != 0 {
		t.Errorf("Reviewer = %q, Mentions = %v, want the requested reviewer's agent and no mention", b.Reviewer, b.Mentions)
	}
	if got, want := e.Channel, "github://urmzd/fiber/pull/77?head_sha=deadbeef"; got != want {
		t.Errorf("Channel = %q, want %q", got, want)
	}
	// With no message, the pull request is the request.
	if !strings.HasPrefix(b.Text, "Coalesce sink edits") || !strings.Contains(b.Text, "flushes on an interval") {
		t.Errorf("Text = %q, want the pull request title and body", b.Text)
	}
	// A mention of a human in the description is left alone.
	if !strings.Contains(b.Text, "@urmzd") {
		t.Errorf("Text = %q, want the human mention preserved", b.Text)
	}
	if !b.Pull || b.Number != 77 || b.BaseRef != "main" || b.HeadRef != "coalesce-edits" || b.HeadSHA != "deadbeef" {
		t.Errorf("ref = %+v", b.GitHubRef)
	}
}

func TestAReviewRequestedFromAHumanPublishesNothing(t *testing.T) {
	body := strings.Replace(reviewRequested,
		`"requested_reviewer": {"login": "docs-bot[bot]", "type": "Bot"}`,
		`"requested_reviewer": {"login": "urmzd", "type": "User"}`, 1)

	c, tags := start(t, github.Config{Bots: map[string]string{"docs-bot": "docs-bot"}})
	w := httptest.NewRecorder()
	c.ServeHTTP(w, deliver(t, "pull_request", "d4", body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	select {
	case e := <-tags:
		t.Errorf("a review request from a human published %+v", e)
	default:
	}
}

func TestDeliveriesThatPublishNothing(t *testing.T) {
	tests := []struct {
		name  string
		event string
		body  string
	}{
		{
			name:  "our own comment coming back, which is how a mesh loops forever",
			event: "issue_comment",
			body:  strings.Replace(issueComment, `"sender": {"login": "urmzd", "type": "User"}`, `"sender": {"login": "docs-bot[bot]", "type": "Bot"}`, 1),
		},
		{
			name:  "an edited comment, because editing is not asking again",
			event: "issue_comment",
			body:  strings.Replace(issueComment, `"action": "created"`, `"action": "edited"`, 1),
		},
		{
			name:  "a comment mentioning nobody we run",
			event: "issue_comment",
			body:  strings.ReplaceAll(issueComment, "@docs-bot", "@urmzd"),
		},
		{
			name:  "a push, which names no agent and would run one per commit",
			event: "push",
			body:  `{"repository":{"name":"fiber","owner":{"login":"urmzd"}},"sender":{"login":"urmzd","type":"User"}}`,
		},
		{
			name:  "a pull request being closed",
			event: "pull_request",
			body:  strings.Replace(reviewRequested, `"action": "review_requested"`, `"action": "closed"`, 1),
		},
		{
			name:  "an owner that could not be an address workspace",
			event: "issue_comment",
			body:  strings.ReplaceAll(issueComment, `"login": "urmzd", "type": "User"`, `"login": "urm/zd", "type": "User"`),
		},
	}

	c, tags := start(t, github.Config{
		Agents: mention.NewNames("docs-bot"),
		Bots:   map[string]string{"docs-bot": "docs-bot"},
	})
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c.ServeHTTP(w, deliver(t, tc.event, "d-"+tc.name, tc.body))
			if w.Code != http.StatusOK {
				t.Errorf("status = %d, want 200 so GitHub stops retrying", w.Code)
			}
			select {
			case e := <-tags:
				t.Errorf("published %+v", e)
			default:
			}
		})
	}
}

func TestAnIssueOpenedWithAMentionIsPublished(t *testing.T) {
	body := `{
	  "action": "opened",
	  "issue": {
	    "number": 9, "title": "@docs-bot please triage this", "body": "The sink posts twice on retry.",
	    "state": "open", "user": {"login": "urmzd", "type": "User"},
	    "created_at": "2026-07-22T08:00:00Z", "updated_at": "2026-07-22T08:00:00Z"
	  },
	  "repository": {"name": "fiber", "full_name": "urmzd/fiber", "owner": {"login": "urmzd", "type": "User"}},
	  "sender": {"login": "urmzd", "type": "User"}
	}`

	c, tags := start(t, github.Config{Agents: mention.NewNames("docs-bot")})
	w := httptest.NewRecorder()
	c.ServeHTTP(w, deliver(t, "issues", "d5", body))
	e := receive(t, tags)
	r := request(t, e)

	if e.Type != event.TypeGitHubIssueOpened || !slices.Equal(r.Mentions, []string{"docs-bot"}) {
		t.Errorf("type = %s, Mentions = %v", e.Type, r.Mentions)
	}
	if r.Text != "please triage this\n\nThe sink posts twice on retry." {
		t.Errorf("Text = %q", r.Text)
	}
	if got, want := e.Channel, "github://urmzd/fiber/issues/9"; got != want {
		t.Errorf("Channel = %q, want %q", got, want)
	}
}

func TestAnUnsignedDeliveryIsRefusedBeforeItIsParsed(t *testing.T) {
	c, tags := start(t, github.Config{Agents: mention.NewNames("docs-bot")})

	r := deliver(t, "issue_comment", "d6", issueComment)
	r.Header.Del(signature.HeaderGitHubSignature)
	w := httptest.NewRecorder()
	c.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}

	// A signature over different bytes must not pass either.
	r = deliver(t, "issue_comment", "d7", issueComment)
	r.Header.Set(signature.HeaderGitHubSignature, "sha256="+strings.Repeat("ab", 32))
	w = httptest.NewRecorder()
	c.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("tampered status = %d, want 401", w.Code)
	}

	select {
	case tag := <-tags:
		t.Errorf("an unauthenticated delivery raised a tag: %+v", tag)
	default:
	}
}

func TestThePingGitHubSendsWhenAHookIsCreatedIsAnswered(t *testing.T) {
	c, _ := start(t, github.Config{Agents: mention.NewNames("docs-bot")})
	w := httptest.NewRecorder()
	c.ServeHTTP(w, deliver(t, "ping", "d8", `{"zen":"Non-blocking is better than blocking.","hook_id":1}`))
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

// ── Sink ────────────────────────────────────────────────────────────

var target = address.MustParse("github://urmzd/fiber/issues/42")

func ev(seq uint64, typ, data string) event.Envelope {
	if data == "" {
		data = "{}"
	}
	body, err := event.Decode(typ, json.RawMessage(data))
	if err != nil {
		panic(err)
	}
	return event.Envelope{
		ID:      fmt.Sprintf("run_1:%d", seq),
		Source:  "fiber://run/run_1",
		Type:    typ,
		Channel: "github://urmzd/fiber",
		Tenant:  "acme",
		Run:     "run_1",
		Seq:     seq,
		Agent:   "docs-bot",
		Version: "1",
		Time:    time.Unix(1700000100, 0).Add(time.Duration(seq) * time.Second),
		Data:    body,
	}
}

func textEvent(seq uint64, s string) event.Envelope {
	b, err := json.Marshal(s)
	if err != nil {
		t := &testing.T{}
		t.Fatal(err)
	}
	return ev(seq, event.TypeText, `{"text":`+string(b)+`}`)
}

func sinkConnector(t *testing.T, api github.API) *github.Connector {
	t.Helper()
	c, err := github.New(github.Config{API: api, Owners: []string{"urmzd"}, Interval: -1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestDeliveringTheSameEventsTwiceKeepsOneEditedComment(t *testing.T) {
	fake := github.NewFake()
	c := sinkConnector(t, fake)
	ctx := context.Background()

	events := []event.Envelope{
		ev(1, event.TypeAccepted, ""),
		textEvent(2, "The retry path posts twice "),
		textEvent(3, "because the handle was lost."),
		ev(4, event.TypeCitation, `{"source":{"document_title":"sink.go","source_uri":"https://github.com/urmzd/saige/flow/blob/main/sink.go"}}`),
		ev(5, event.TypeCompleted, ""),
	}
	for range 2 {
		for _, e := range events {
			if err := c.Deliver(ctx, target, e); err != nil {
				t.Fatalf("Deliver(seq %d): %v", e.Seq, err)
			}
		}
	}

	comments := fake.Comments()
	if len(comments) != 1 {
		t.Fatalf("github holds %d comments, want exactly 1", len(comments))
	}
	comment := comments[0]
	if comment.Ref != (github.Ref{Owner: "urmzd", Repo: "fiber", Number: 42}) {
		t.Errorf("comment ref = %+v", comment.Ref)
	}
	if got := strings.Count(comment.Body, "The retry path posts twice because the handle was lost."); got != 1 {
		t.Errorf("body contains the answer %d times, want 1:\n%s", got, comment.Body)
	}
	if !strings.Contains(comment.Body, "**docs-bot** _(done)_") {
		t.Errorf("body is not GitHub markdown:\n%s", comment.Body)
	}
	if !strings.Contains(comment.Body, "[1] [sink.go](https://github.com/urmzd/saige/flow/blob/main/sink.go)") {
		t.Errorf("citation is not a markdown footnote:\n%s", comment.Body)
	}
	if comment.Edits == 0 {
		t.Error("the comment was never edited, so nothing streamed")
	}
}

func TestOutOfOrderDeliveryDoesNotRenderStaleTextOverFresh(t *testing.T) {
	fake := github.NewFake()
	c := sinkConnector(t, fake)
	ctx := context.Background()

	for _, e := range []event.Envelope{textEvent(3, "second"), textEvent(2, "first "), ev(4, event.TypeCompleted, "")} {
		if err := c.Deliver(ctx, target, e); err != nil {
			t.Fatal(err)
		}
	}
	if body := fake.Comments()[0].Body; !strings.Contains(body, "first second") {
		t.Errorf("body = %q, want sequence order", body)
	}
}

func TestTheAnswersOwnMarkdownIsPassedThroughUnescaped(t *testing.T) {
	fake := github.NewFake()
	c := sinkConnector(t, fake)
	ctx := context.Background()

	answer := "Here is the fix:\n\n```go\nif err != nil {\n\treturn err\n}\n```\n\nSee **sink.go**."
	if err := c.Deliver(ctx, target, textEvent(1, answer)); err != nil {
		t.Fatal(err)
	}
	if body := fake.Comments()[0].Body; !strings.Contains(body, answer) {
		t.Errorf("the answer was rewritten:\n%s", body)
	}
}

func TestDeliveryToAnUnservedTargetIsRefused(t *testing.T) {
	c := sinkConnector(t, github.NewFake())
	ctx := context.Background()

	tests := []struct{ name, target string }{
		{name: "another owner", target: "github://someone-else/fiber/issues/1"},
		{name: "another connector", target: "slack://T01/C02"},
		{name: "no number", target: "github://urmzd/fiber/issues"},
		{name: "not a number", target: "github://urmzd/fiber/issues/latest"},
		{name: "an unknown resource kind", target: "github://urmzd/fiber/discussions/4"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := c.Deliver(ctx, address.MustParse(tc.target), textEvent(1, "hi"))
			if err == nil || !strings.Contains(err.Error(), "undeliverable") {
				t.Errorf("Deliver error = %v, want ErrUndeliverable", err)
			}
		})
	}
}

// ── Actor ───────────────────────────────────────────────────────────

func TestActionsChangeGitHubAndReportWhatTheyChanged(t *testing.T) {
	fake := github.NewFake()
	c := sinkConnector(t, fake)
	ctx := context.Background()

	actions := map[string]connector.Action{}
	for _, a := range c.Actions() {
		actions[a.Name] = a
	}
	if len(actions) != 7 {
		t.Fatalf("actions = %v, want seven", actions)
	}

	res, err := actions["github_comment"].Invoke(ctx, target, json.RawMessage(`{"body":"looked into it"}`))
	if err != nil {
		t.Fatalf("github_comment: %v", err)
	}
	if got := res.Address.String(); !strings.HasPrefix(got, "github://urmzd/fiber/issues/42?comment=") {
		t.Errorf("Result.Address = %q, want the comment it created", got)
	}
	if len(fake.Comments()) != 1 || fake.Comments()[0].Body != "looked into it" {
		t.Errorf("comments = %+v", fake.Comments())
	}

	pull := address.MustParse("github://urmzd/fiber/pull/77")
	if _, err := actions["github_request_review"].Invoke(ctx, pull, json.RawMessage(`{"reviewers":["urmzd"],"teams":["platform"]}`)); err != nil {
		t.Fatalf("github_request_review: %v", err)
	}
	if got := fake.Reviewers(github.Ref{Owner: "urmzd", Repo: "fiber", Number: 77}); len(got) != 2 {
		t.Errorf("reviewers = %v, want the user and the team", got)
	}

	if _, err := actions["github_add_labels"].Invoke(ctx, target, json.RawMessage(`{"labels":["bug","connectors"]}`)); err != nil {
		t.Fatalf("github_add_labels: %v", err)
	}
	// Adding the same label again is safe, which is what makes the action
	// retryable.
	if _, err := actions["github_add_labels"].Invoke(ctx, target, json.RawMessage(`{"labels":["bug"]}`)); err != nil {
		t.Fatalf("github_add_labels again: %v", err)
	}
	if got := fake.Labels(github.Ref{Owner: "urmzd", Repo: "fiber", Number: 42}); len(got) != 2 {
		t.Errorf("labels = %v, want two", got)
	}
}

func TestEveryActionIsSelfDescribing(t *testing.T) {
	c := sinkConnector(t, github.NewFake())
	for _, a := range c.Actions() {
		t.Run(a.Name, func(t *testing.T) {
			if !strings.HasPrefix(a.Name, "github_") {
				t.Errorf("Name = %q, want the connector prefix", a.Name)
			}
			if a.Description == "" || a.Invoke == nil {
				t.Error("an action a model cannot understand or call")
			}
			var schema map[string]any
			if err := json.Unmarshal(a.Schema, &schema); err != nil {
				t.Fatalf("schema is not valid JSON: %v", err)
			}
			if schema["type"] != "object" {
				t.Errorf("schema type = %v, want object", schema["type"])
			}
		})
	}
}

func TestAnActionCannotReachIntoAnotherRepository(t *testing.T) {
	c := sinkConnector(t, github.NewFake())
	other := address.MustParse("github://someone-else/private/issues/1")
	args := json.RawMessage(`{"body":"x","labels":["x"],"reviewers":["x"]}`)
	for _, a := range c.Actions() {
		if _, err := a.Invoke(context.Background(), other, args); err == nil || !strings.Contains(err.Error(), "undeliverable") {
			t.Errorf("%s against another owner: error = %v, want ErrUndeliverable", a.Name, err)
		}
	}
}

func TestGitHubImplementsAllThreeFaces(t *testing.T) {
	roles := connector.RolesOf(sinkConnector(t, github.NewFake()))
	if !roles.Trigger || !roles.Sink || !roles.Actor {
		t.Errorf("RolesOf(github) = %+v, want all three faces", roles)
	}
}

// receive waits for an event and checks it is valid and renders through the
// event catalog as its request text.
func receive(t *testing.T, tags <-chan event.Envelope) event.Envelope {
	t.Helper()
	select {
	case e := <-tags:
		if err := e.Validate(); err != nil {
			t.Fatalf("published an invalid event: %v", err)
		}
		rendertest.Check(t, e)
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("no event was published")
		return event.Envelope{}
	}
}

// request is the request e carries.
func request(t *testing.T, e event.Envelope) event.Request {
	t.Helper()
	r, ok := event.RequestOf(e.Data)
	if !ok {
		t.Fatalf("%s carries no request", e.Type)
	}
	return r
}

// ref is the GitHub reference e's body embeds.
func ref(t *testing.T, e event.Envelope) event.GitHubRef {
	t.Helper()
	raw, err := json.Marshal(e.Data)
	if err != nil {
		t.Fatal(err)
	}
	var r event.GitHubRef
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	return r
}
