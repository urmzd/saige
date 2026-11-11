package slack_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/urmzd/saige/flow/connectors/slack"
	"github.com/urmzd/saige/flow/event"
)

// invocation is a slash command request as Slack documents it.
func invocation(overrides map[string]string) string {
	form := url.Values{
		"token":        {"XXYYZZ"},
		"team_id":      {"T01ABCDEF"},
		"team_domain":  {"acme"},
		"channel_id":   {"C09CODE"},
		"channel_name": {"fix-flaky-login"},
		"user_id":      {"U02HUMAN"},
		"user_name":    {"alice"},
		"command":      {"/create-pr"},
		"text":         {"Fix the flaky login test"},
		"api_app_id":   {"A01APP"},
		"response_url": {"https://hooks.slack.com/commands/T01ABCDEF/1/abc"},
		"trigger_id":   {"13345224609.738474920.8088930838d88f008e0"},
	}
	for k, v := range overrides {
		if v == "" {
			form.Del(k)
			continue
		}
		form.Set(k, v)
	}
	return form.Encode()
}

func command(t *testing.T, body string) *http.Request {
	t.Helper()
	r := post(t, body)
	r.URL.Path = slack.CommandsPath
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func TestASlashCommandPublishesAnInvocationOnItsChannel(t *testing.T) {
	c, tags := start(t, slack.Config{Tenant: "acme"})

	w := httptest.NewRecorder()
	c.ServeHTTP(w, command(t, invocation(nil)))
	if w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Fatalf("status = %d body = %q, want an empty 200", w.Code, w.Body)
	}

	e := receive(t, tags)
	b, ok := e.Data.(event.SlackCommandInvoked)
	if !ok {
		t.Fatalf("published %s, want %s", e.Type, event.TypeSlackCommandInvoked)
	}
	if e.ID != "command:13345224609.738474920.8088930838d88f008e0" || e.Tenant != "acme" {
		t.Errorf("id, tenant = %q, %q", e.ID, e.Tenant)
	}
	if b.Text != "/create-pr Fix the flaky login test" {
		t.Errorf("text = %q, want the invocation as a human would have typed it", b.Text)
	}
	if e.Channel != "slack://T01ABCDEF/C09CODE" || e.Source != e.Channel {
		t.Errorf("channel = %q source = %q, want the code channel with no thread", e.Channel, e.Source)
	}
	if b.Actor.ID != "U02HUMAN" || b.User != "U02HUMAN" {
		t.Errorf("actor = %q user = %q", b.Actor.ID, b.User)
	}
	if b.Command != "/create-pr" || b.Team != "T01ABCDEF" || b.Channel != "C09CODE" || b.AppID != "A01APP" {
		t.Errorf("body = %+v", b)
	}
	if len(b.Mentions) != 0 {
		t.Errorf("mentions = %v, want none: the channel's members decide", b.Mentions)
	}
	if !e.Time.Equal(signedAt) {
		t.Errorf("time = %v, want the signed request time %v", e.Time, signedAt)
	}
}

func TestReplayingAnInvocationPublishesTheSameEvent(t *testing.T) {
	c, tags := start(t, slack.Config{})

	var ids []string
	for range 2 {
		c.ServeHTTP(httptest.NewRecorder(), command(t, invocation(nil)))
		ids = append(ids, (<-tags).ID)
	}
	if ids[0] != ids[1] {
		t.Errorf("ids = %v, want one idempotency key for one invocation", ids)
	}
}

func TestASlashCommandCanMentionABot(t *testing.T) {
	c, tags := start(t, slack.Config{Bots: map[string]string{"U08REL": "release-bot"}})

	c.ServeHTTP(httptest.NewRecorder(), command(t, invocation(map[string]string{"text": "<@U08REL> cut 1.2"})))

	b := receive(t, tags).Data.(event.SlackCommandInvoked)
	if !slices.Equal(b.Mentions, []string{"release-bot"}) || b.Text != "/create-pr cut 1.2" {
		t.Errorf("mentions = %v text = %q", b.Mentions, b.Text)
	}
}

func TestAnUnsignedSlashCommandIsRefusedBeforeItIsParsed(t *testing.T) {
	c, tags := start(t, slack.Config{})

	cases := map[string]func(*http.Request){
		"no signature":    func(r *http.Request) { r.Header.Del("X-Slack-Signature") },
		"wrong signature": func(r *http.Request) { r.Header.Set("X-Slack-Signature", "v0="+strings.Repeat("0", 64)) },
		"tampered body": func(r *http.Request) {
			*r = *command(t, invocation(nil))
			r.Body = http.NoBody
		},
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			r := command(t, invocation(nil))
			tamper(r)
			w := httptest.NewRecorder()
			c.ServeHTTP(w, r)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", w.Code)
			}
		})
	}
	select {
	case e := <-tags:
		t.Errorf("an unauthenticated command published %+v", e)
	default:
	}
}

func TestMalformedInvocationsPublishNothing(t *testing.T) {
	c, tags := start(t, slack.Config{})

	for _, missing := range []string{"team_id", "channel_id", "user_id", "trigger_id", "command"} {
		t.Run(missing, func(t *testing.T) {
			w := httptest.NewRecorder()
			c.ServeHTTP(w, command(t, invocation(map[string]string{missing: ""})))
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", w.Code)
			}
		})
	}
	select {
	case e := <-tags:
		t.Errorf("a malformed command published %+v", e)
	default:
	}
}
