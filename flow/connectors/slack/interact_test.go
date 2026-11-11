package slack_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/urmzd/saige/flow/connectors/slack"
)

// interactivity posts a payload to the interactivity URL, signed as Slack
// signs it.
func interactivity(t *testing.T, payload string) *http.Request {
	t.Helper()
	r := post(t, url.Values{"payload": {payload}}.Encode())
	r.URL.Path = "/slack" + slack.InteractivityPath
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

const blockActions = `{"type":"block_actions","team":{"id":"T01ABCDEF"},"user":{"id":"U0HUMAN1"},"trigger_id":"tr-1",
 "container":{"type":"message","channel_id":"C02CHANNEL","message_ts":"1700000100.000200","thread_ts":"1700000000.000100"},
 "channel":{"id":"C02CHANNEL"},"message":{"ts":"1700000100.000200","thread_ts":"1700000000.000100","text":"Approve?"},
 "actions":[{"action_id":"aui.approve#1","block_id":"approval:a1:r1","value":"{\"target\":\"a1\"}"}]}`

const messageShortcut = `{"type":"message_action","callback_id":"aui_ask_about","team":{"id":"T01ABCDEF"},"user":{"id":"U0HUMAN1"},"trigger_id":"tr-2",
 "channel":{"id":"C02CHANNEL"},"message":{"ts":"1700000200.000300","text":"Why did the deploy fail?"}}`

// A press on the app's message and a message shortcut reach the host as
// interactions, signed requests only, and only for this connector's team.
func TestInteractionsReachTheHost(t *testing.T) {
	var mu sync.Mutex
	var got []slack.Interaction
	c, _ := start(t, slack.Config{Team: "T01ABCDEF", Interact: func(_ context.Context, in slack.Interaction) error {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, in)
		return nil
	}})
	for _, payload := range []string{blockActions, messageShortcut} {
		w := httptest.NewRecorder()
		c.ServeHTTP(w, interactivity(t, payload))
		if w.Code != http.StatusOK {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	want := []slack.Interaction{
		{Kind: slack.InteractAction, Team: "T01ABCDEF", Channel: "C02CHANNEL", User: "U0HUMAN1", MessageTS: "1700000100.000200", ThreadTS: "1700000000.000100", ActionID: "aui.approve#1", BlockID: "approval:a1:r1", Value: `{"target":"a1"}`, TriggerID: "tr-1"},
		{Kind: slack.InteractMessageShortcut, Team: "T01ABCDEF", Channel: "C02CHANNEL", User: "U0HUMAN1", MessageTS: "1700000200.000300", CallbackID: "aui_ask_about", Text: "Why did the deploy fail?", TriggerID: "tr-2"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("interaction %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// Unsigned, it is refused before it is read.
	r := interactivity(t, blockActions)
	r.Header.Set("X-Slack-Signature", "v0=0000")
	w := httptest.NewRecorder()
	c.ServeHTTP(w, r)
	if w.Code == http.StatusOK || len(got) != 2 {
		t.Fatalf("an unsigned press was taken: %d", w.Code)
	}
}

func TestInteractionsFromAnotherTeamOrAFailedHost(t *testing.T) {
	calls := 0
	c, _ := start(t, slack.Config{Team: "T0OTHER", Interact: func(context.Context, slack.Interaction) error { calls++; return nil }})
	w := httptest.NewRecorder()
	c.ServeHTTP(w, interactivity(t, blockActions))
	if w.Code != http.StatusOK || calls != 0 {
		t.Fatalf("another team's press: status %d, calls %d", w.Code, calls)
	}
	failing, _ := start(t, slack.Config{Interact: func(context.Context, slack.Interaction) error { return errors.New("down") }})
	w = httptest.NewRecorder()
	failing.ServeHTTP(w, interactivity(t, blockActions))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("a failed host answered %d", w.Code)
	}
	w = httptest.NewRecorder()
	failing.ServeHTTP(w, interactivity(t, "{"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a malformed payload answered %d", w.Code)
	}
}

// Over Socket Mode, an interactive envelope reaches the host and is
// acknowledged.
func TestSocketModeInteractionsReachTheHost(t *testing.T) {
	acked := make(chan string, 1)
	f := newFakeSocketSlack(t, func(ctx context.Context, f *fakeSocketSlack, conn *websocket.Conn, n int) {
		f.send(ctx, conn, map[string]any{"type": "hello"})
		if n == 1 {
			f.send(ctx, conn, map[string]any{"type": "interactive", "envelope_id": "env-9", "payload": json.RawMessage(blockActions)})
			acked <- f.ackOf(ctx, conn)
		}
		<-ctx.Done()
	})
	pressed := make(chan slack.Interaction, 1)
	start(t, slack.Config{Socket: f.config(nil), Interact: func(_ context.Context, in slack.Interaction) error {
		pressed <- in
		return nil
	}})
	// Whatever this test returns with, it expects no more of the fake.
	defer f.quiet()
	select {
	case in := <-pressed:
		if in.ActionID != "aui.approve#1" || in.User != "U0HUMAN1" {
			t.Fatalf("pressed %+v", in)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the press did not reach the host")
	}
	select {
	case id := <-acked:
		if id != "env-9" {
			t.Fatalf("acknowledged %q", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the press was not acknowledged")
	}
}
