package slack_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/flow/connectors/internal/rendertest"
	"github.com/urmzd/saige/flow/event"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connector"
	"github.com/urmzd/saige/flow/connectors/slack"
	"github.com/urmzd/saige/flow/signature"
)

const secret = "8f742231b10e8888abcd99yyyzzz85a5"

var signedAt = time.Unix(1700000100, 0)

// post builds a request signed the way Slack signs one. Using the real verifier
// from pkg/signature rather than a permissive double is deliberate: the trigger's
// only security boundary is this check, and a test that stubbed it would prove
// the parsing works while saying nothing about whether the boundary is wired up.
func post(t *testing.T, body string) *http.Request {
	t.Helper()
	ts := strconv.FormatInt(signedAt.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:" + ts + ":" + body))

	r := httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(signature.HeaderSlackTimestamp, ts)
	r.Header.Set(signature.HeaderSlackSignature, "v0="+hex.EncodeToString(mac.Sum(nil)))
	return r
}

func verifier() *signature.Slack {
	return &signature.Slack{Secret: []byte(secret), Now: func() time.Time { return signedAt }}
}

// start brings up a connector with an Ingest running, and returns the channel
// its events are published on.
func start(t *testing.T, cfg slack.Config) (*slack.Connector, <-chan event.Envelope) {
	t.Helper()
	if cfg.API == nil {
		cfg.API = slack.NewFake()
	}
	if cfg.Verifier == nil {
		cfg.Verifier = verifier()
	}
	c, err := slack.New(cfg)
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
	// Wait for Ingest to claim the channel before the first request.
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

// appMention is a realistic app_mention delivery: a mention mid-sentence, in a
// thread, with Slack's own entity escaping and markup.
const appMention = `{
  "token": "z26uFbvR1xHJEdHE1OQiO6t8",
  "team_id": "T01ABCDEF",
  "api_app_id": "A08APPID",
  "event": {
    "type": "app_mention",
    "user": "U0HUMAN1",
    "text": "hey <@U08BOTID> can you summarize <https://example.com/design|the design doc> for <@U0HUMAN2>? &lt;3",
    "ts": "1700000090.001200",
    "team": "T01ABCDEF",
    "thread_ts": "1700000000.000100",
    "channel": "C02CHANNEL",
    "event_ts": "1700000090.001200"
  },
  "type": "event_callback",
  "event_id": "Ev08MNEMONIC",
  "event_time": 1700000090,
  "authorizations": [{"enterprise_id": null, "team_id": "T01ABCDEF", "user_id": "U08BOTID", "is_bot": true}],
  "is_ext_shared_channel": false
}`

func TestAppMentionPublishesAMessageNamingTheAgentItMentioned(t *testing.T) {
	c, tags := start(t, slack.Config{
		Team:   "T01ABCDEF",
		Tenant: "acme",
		Bots:   map[string]string{"U08BOTID": "docs-bot"},
	})

	w := httptest.NewRecorder()
	c.ServeHTTP(w, post(t, appMention))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", w.Code, w.Body.String())
	}

	e := receive(t, tags)
	b := posted(t, e)
	if e.ID != "Ev08MNEMONIC" {
		t.Errorf("ID = %q, want Slack's event_id so a retry dedupes", e.ID)
	}
	if e.Type != event.TypeMessagePosted || e.Tenant != "acme" || e.Subject != "C02CHANNEL" {
		t.Errorf("type, tenant, subject = %q, %q, %q", e.Type, e.Tenant, e.Subject)
	}
	if want := []string{"docs-bot"}; !slices.Equal(b.Mentions, want) {
		t.Errorf("Mentions = %v, want %v", b.Mentions, want)
	}
	// The mention is gone, the link is its label, the other human survives as a
	// mention, and the entity is decoded.
	want := "hey can you summarize the design doc for @U0HUMAN2? <3"
	if b.Text != want {
		t.Errorf("Text = %q, want %q", b.Text, want)
	}
	thread := "slack://T01ABCDEF/C02CHANNEL?thread=1700000000.000100"
	if e.Source != thread || e.Channel != thread {
		t.Errorf("Source, Channel = %q, %q, want both %q", e.Source, e.Channel, thread)
	}
	if b.Actor.ID != "U0HUMAN1" {
		t.Errorf("Actor.ID = %q, want the human who typed it", b.Actor.ID)
	}
	if !e.Time.Equal(time.Unix(1700000090, 1200000).UTC()) {
		t.Errorf("Time = %s, want the event timestamp with its microseconds", e.Time)
	}
	wantRef := event.SlackRef{
		Team: "T01ABCDEF", Channel: "C02CHANNEL", User: "U0HUMAN1", Event: "app_mention",
		TS: "1700000090.001200", ThreadTS: "1700000000.000100", AppID: "A08APPID",
	}
	if b.Slack == nil || *b.Slack != wantRef {
		t.Errorf("Slack = %+v, want %+v", b.Slack, wantRef)
	}
}

func TestSourceAddressRoundTripsThroughTheAddressPackage(t *testing.T) {
	c, tags := start(t, slack.Config{Bots: map[string]string{"U08BOTID": "docs-bot"}})
	w := httptest.NewRecorder()
	c.ServeHTTP(w, post(t, appMention))
	e := receive(t, tags)

	parsed, err := address.Parse(e.Channel)
	if err != nil {
		t.Fatalf("Parse(%q): %v", e.Channel, err)
	}
	if parsed.String() != e.Channel {
		t.Errorf("round trip = %q, want %q", parsed, e.Channel)
	}
	if parsed.Connector != "slack" || parsed.Workspace != "T01ABCDEF" || parsed.Resource() != "C02CHANNEL" {
		t.Errorf("parsed = %+v, want the slack team and channel", parsed)
	}
	if thread, ok := parsed.Param("thread"); !ok || thread != "1700000000.000100" {
		t.Errorf("thread param = %q (%v), want the thread timestamp", thread, ok)
	}
}

func TestAMentionWithoutAThreadAnswersUnderTheMentionItself(t *testing.T) {
	body := strings.ReplaceAll(appMention, `"thread_ts": "1700000000.000100",`, "")
	c, tags := start(t, slack.Config{Bots: map[string]string{"U08BOTID": "docs-bot"}})
	w := httptest.NewRecorder()
	c.ServeHTTP(w, post(t, body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	e := receive(t, tags)
	if !strings.HasSuffix(e.Channel, "?thread=1700000090.001200") {
		t.Errorf("channel = %q, want the mention's own ts so the answer opens a thread", e.Channel)
	}
}

func TestAMentionOfNoKnownBotPublishesWithNoMentions(t *testing.T) {
	body := strings.ReplaceAll(appMention,
		`"text": "hey <@U08BOTID> can you summarize <https://example.com/design|the design doc> for <@U0HUMAN2>? &lt;3"`,
		`"text": "<@U08APPID> @triage please look at this"`)

	// The app's own user is not mapped to an agent, and an @name in the text
	// is not a mention: the channel's members decide who answers.
	c, tags := start(t, slack.Config{Bots: map[string]string{"U08BOTID": "docs-bot"}})
	w := httptest.NewRecorder()
	c.ServeHTTP(w, post(t, body))
	b := posted(t, receive(t, tags))

	if len(b.Mentions) != 0 {
		t.Errorf("Mentions = %v, want none", b.Mentions)
	}
	if b.Text != "@U08APPID @triage please look at this" {
		t.Errorf("Text = %q", b.Text)
	}
}

func TestEventsThatPublishNothing(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "a message from a bot, which is how a mesh would answer itself",
			body: `{"type":"event_callback","team_id":"T01ABCDEF","event_id":"Ev1","event":{
				"type":"app_mention","bot_id":"B08SELF","user":"U08BOTID","text":"<@U08BOTID> hi",
				"channel":"C02CHANNEL","ts":"1700000090.000100"}}`,
		},
		{
			name: "a plain channel message, because joining a channel is not subscribing to it",
			body: `{"type":"event_callback","team_id":"T01ABCDEF","event_id":"Ev2","event":{
				"type":"message","channel_type":"channel","user":"U0HUMAN1","text":"<@U08BOTID> hi",
				"channel":"C02CHANNEL","ts":"1700000090.000100"}}`,
		},
		{
			name: "a message subtype: a join, an edit, a file share",
			body: `{"type":"event_callback","team_id":"T01ABCDEF","event_id":"Ev3","event":{
				"type":"message","channel_type":"im","subtype":"message_changed","user":"U0HUMAN1",
				"text":"<@U08BOTID> hi","channel":"D02DM","ts":"1700000090.000100"}}`,
		},
		{
			name: "an event type this connector does not act on",
			body: `{"type":"event_callback","team_id":"T01ABCDEF","event_id":"Ev4","event":{
				"type":"reaction_added","user":"U0HUMAN1","channel":"C02CHANNEL","ts":"1700000090.000100"}}`,
		},
		{
			name: "a team id that could not be an address workspace",
			body: `{"type":"event_callback","team_id":"T01:BAD","event_id":"Ev6","event":{
				"type":"app_mention","user":"U0HUMAN1","text":"<@U08BOTID> hi",
				"channel":"C02CHANNEL","ts":"1700000090.000100"}}`,
		},
	}

	c, tags := start(t, slack.Config{Bots: map[string]string{"U08BOTID": "docs-bot"}})
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c.ServeHTTP(w, post(t, tc.body))
			// Acknowledged, so Slack does not retry an event we will never want.
			if w.Code != http.StatusOK {
				t.Errorf("status = %d, want 200", w.Code)
			}
			select {
			case e := <-tags:
				t.Errorf("published %+v", e)
			default:
			}
		})
	}
}

func TestADirectMessageIsAMentionByContext(t *testing.T) {
	body := `{"type":"event_callback","team_id":"T01ABCDEF","event_id":"EvDM","event":{
		"type":"message","channel_type":"im","user":"U0HUMAN1","text":"what changed yesterday?",
		"channel":"D02DM","ts":"1700000090.000100","event_ts":"1700000090.000100"}}`

	c, tags := start(t, slack.Config{})
	w := httptest.NewRecorder()
	c.ServeHTTP(w, post(t, body))
	e := receive(t, tags)
	b := posted(t, e)

	// The adapter chooses no agent: the members of the direct message's
	// channel decide who answers.
	if len(b.Mentions) != 0 {
		t.Errorf("Mentions = %v, want none", b.Mentions)
	}
	if b.Text != "what changed yesterday?" {
		t.Errorf("Text = %q", b.Text)
	}
	if b.Slack.ChannelType != "im" {
		t.Errorf("ChannelType = %q, want im", b.Slack.ChannelType)
	}
	if e.Channel != "slack://T01ABCDEF/D02DM?thread=1700000090.000100" {
		t.Errorf("Channel = %q", e.Channel)
	}
}

func TestAnUnsignedOrTamperedRequestIsRefusedBeforeItIsParsed(t *testing.T) {
	c, tags := start(t, slack.Config{Bots: map[string]string{"U08BOTID": "docs-bot"}})

	tests := []struct {
		name    string
		mutate  func(*http.Request)
		wantSts int
	}{
		{
			name:    "no signature at all",
			mutate:  func(r *http.Request) { r.Header.Del(signature.HeaderSlackSignature) },
			wantSts: http.StatusUnauthorized,
		},
		{
			name:    "a signature over different bytes",
			mutate:  func(r *http.Request) { r.Header.Set(signature.HeaderSlackSignature, "v0="+strings.Repeat("ab", 32)) },
			wantSts: http.StatusUnauthorized,
		},
		{
			name:    "a stale timestamp, outside the replay window",
			mutate:  func(r *http.Request) { r.Header.Set(signature.HeaderSlackTimestamp, "1600000000") },
			wantSts: http.StatusUnauthorized,
		},
		{
			name:    "a method Slack never uses",
			mutate:  func(r *http.Request) { r.Method = http.MethodGet },
			wantSts: http.StatusMethodNotAllowed,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := post(t, appMention)
			tc.mutate(r)
			w := httptest.NewRecorder()
			c.ServeHTTP(w, r)
			if w.Code != tc.wantSts {
				t.Errorf("status = %d, want %d", w.Code, tc.wantSts)
			}
			select {
			case e := <-tags:
				t.Errorf("an unauthenticated request published %+v", e)
			default:
			}
		})
	}
}

func TestAConnectorWithNoVerifierRefusesEveryRequest(t *testing.T) {
	c, err := slack.New(slack.Config{API: slack.NewFake()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w := httptest.NewRecorder()
	c.ServeHTTP(w, post(t, appMention))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500: an unconfigured webhook is broken, not open", w.Code)
	}
}

func TestURLVerificationIsAnsweredButOnlyWhenSigned(t *testing.T) {
	c, _ := start(t, slack.Config{})
	body := `{"type":"url_verification","token":"tok","challenge":"3eZbrw1aB"}`

	w := httptest.NewRecorder()
	c.ServeHTTP(w, post(t, body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["challenge"] != "3eZbrw1aB" {
		t.Errorf("challenge = %q, want it echoed", got["challenge"])
	}

	unsigned := httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(body))
	w = httptest.NewRecorder()
	c.ServeHTTP(w, unsigned)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("unsigned challenge status = %d, want 401", w.Code)
	}
}

func TestARequestArrivingBeforeIngestAsksSlackToRetry(t *testing.T) {
	c, err := slack.New(slack.Config{
		API:      slack.NewFake(),
		Verifier: verifier(),
		Bots:     map[string]string{"U08BOTID": "docs-bot"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// No Ingest is running: the event has nowhere to go, and 503 is the answer
	// that makes Slack redeliver an event we would otherwise drop.
	w := httptest.NewRecorder()
	c.ServeHTTP(w, post(t, appMention))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// ── Sink ────────────────────────────────────────────────────────────

var target = address.MustParse("slack://T01ABCDEF/C02CHANNEL?thread=1700000000.000100")

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
		Channel: "slack://T01ABCDEF/C02CHANNEL",
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
	return ev(seq, event.TypeText, `{"text":`+jsonString(s)+`}`)
}

func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func sinkConnector(t *testing.T, api slack.API) *slack.Connector {
	t.Helper()
	c, err := slack.New(slack.Config{API: api, Team: "T01ABCDEF", Interval: -1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestDeliveringTheSameEventsTwiceKeepsOneEditedMessage(t *testing.T) {
	fake := slack.NewFake()
	c := sinkConnector(t, fake)
	ctx := context.Background()

	events := []event.Envelope{
		ev(1, event.TypeAccepted, ""),
		textEvent(2, "The design doc says "),
		textEvent(3, "three things."),
		ev(4, event.TypeCitation, `{"source":{"document_title":"Design doc","source_uri":"https://example.com/design"}}`),
		ev(5, event.TypeCompleted, ""),
	}
	for range 2 {
		for _, e := range events {
			if err := c.Deliver(ctx, target, e); err != nil {
				t.Fatalf("Deliver(seq %d): %v", e.Seq, err)
			}
		}
	}

	messages := fake.Messages()
	if len(messages) != 1 {
		t.Fatalf("slack holds %d messages, want exactly 1: %+v", len(messages), messages)
	}
	msg := messages[0]
	if msg.Channel != "C02CHANNEL" {
		t.Errorf("channel = %q", msg.Channel)
	}
	if msg.ThreadTS != "1700000000.000100" {
		t.Errorf("thread_ts = %q, want the answer threaded under the mention", msg.ThreadTS)
	}
	if got := strings.Count(msg.Text, "The design doc says three things."); got != 1 {
		t.Errorf("body contains the answer %d times, want 1:\n%s", got, msg.Text)
	}
	if !strings.Contains(msg.Text, "*docs-bot* _(done)_") {
		t.Errorf("body is not mrkdwn:\n%s", msg.Text)
	}
	if !strings.Contains(msg.Text, "[1] <https://example.com/design|Design doc>") {
		t.Errorf("citation is not a mrkdwn footnote link:\n%s", msg.Text)
	}
}

func TestOutOfOrderDeliveryDoesNotRenderStaleTextOverFresh(t *testing.T) {
	fake := slack.NewFake()
	c := sinkConnector(t, fake)
	ctx := context.Background()

	for _, e := range []event.Envelope{
		textEvent(3, "second"),
		textEvent(2, "first "),
		ev(4, event.TypeCompleted, ""),
	} {
		if err := c.Deliver(ctx, target, e); err != nil {
			t.Fatal(err)
		}
	}
	msg := fake.Messages()[0]
	if !strings.Contains(msg.Text, "first second") {
		t.Errorf("body = %q, want the fragments in sequence order", msg.Text)
	}
}

func TestSlackMarkupInAnAnswerIsEscapedRatherThanRendered(t *testing.T) {
	fake := slack.NewFake()
	c := sinkConnector(t, fake)
	ctx := context.Background()

	// A model that echoes a mention must not produce a real one, and a tool
	// result with an angle bracket must not produce broken markup.
	if err := c.Deliver(ctx, target, textEvent(1, "compare <@U0HUMAN1> & a < b")); err != nil {
		t.Fatal(err)
	}
	msg := fake.Messages()[0]
	if strings.Contains(msg.Text, "<@U0HUMAN1>") {
		t.Errorf("an answer produced a live mention:\n%s", msg.Text)
	}
	if !strings.Contains(msg.Text, "&lt;@U0HUMAN1&gt; &amp; a &lt; b") {
		t.Errorf("body = %q, want the prose escaped", msg.Text)
	}
	// Our own markup is not escaped.
	if !strings.Contains(msg.Text, "*docs-bot*") {
		t.Errorf("the header was escaped along with the prose:\n%s", msg.Text)
	}
}

func TestDeliveryToAWorkspaceThisConnectorDoesNotServeIsRefused(t *testing.T) {
	c := sinkConnector(t, slack.NewFake())
	ctx := context.Background()

	tests := []struct {
		name   string
		target string
	}{
		{name: "another workspace", target: "slack://T99OTHER/C02CHANNEL"},
		{name: "another connector's scheme", target: "github://urmzd/fiber/issues/1"},
		{name: "no channel", target: "slack://T01ABCDEF"},
		{name: "too much path", target: "slack://T01ABCDEF/C02CHANNEL/extra"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := c.Deliver(ctx, address.MustParse(tc.target), textEvent(1, "hi"))
			if !isUndeliverable(err) {
				t.Errorf("Deliver error = %v, want ErrUndeliverable", err)
			}
		})
	}
}

func isUndeliverable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "undeliverable")
}

// ── Actor ───────────────────────────────────────────────────────────

func TestActionsPostAndReactAndReportWhatTheyChanged(t *testing.T) {
	fake := slack.NewFake()
	c := sinkConnector(t, fake)
	ctx := context.Background()

	actions := map[string]connector.Action{}
	for _, a := range c.Actions() {
		actions[a.Name] = a
	}
	if len(actions) != 2 {
		t.Fatalf("actions = %v, want two", actions)
	}

	post := actions["slack_post_message"]
	res, err := post.Invoke(ctx, target, json.RawMessage(`{"text":"heads up"}`))
	if err != nil {
		t.Fatalf("slack_post_message: %v", err)
	}
	if res.Address.Connector != "slack" || res.Address.Workspace != "T01ABCDEF" {
		t.Errorf("Result.Address = %s, want the message that was posted", res.Address)
	}
	if _, err := address.Parse(res.Address.String()); err != nil {
		t.Errorf("Result.Address does not round trip: %v", err)
	}
	if res.Summary == "" {
		t.Error("Result.Summary is empty, so the agent learns nothing")
	}
	posted := fake.Messages()
	if len(posted) != 1 || posted[0].Text != "heads up" {
		t.Fatalf("messages = %+v", posted)
	}
	if posted[0].ThreadTS != "1700000000.000100" {
		t.Errorf("thread_ts = %q, want the run's thread by default", posted[0].ThreadTS)
	}

	react := actions["slack_add_reaction"]
	ts := posted[0].TS
	if _, err := react.Invoke(ctx, target, json.RawMessage(`{"name":"eyes","ts":"`+ts+`"}`)); err != nil {
		t.Fatalf("slack_add_reaction: %v", err)
	}
	msg, _ := fake.Message(ts)
	if len(msg.Reactions) != 1 || msg.Reactions[0] != "eyes" {
		t.Errorf("reactions = %v, want [eyes]", msg.Reactions)
	}
}

func TestEveryActionIsSelfDescribing(t *testing.T) {
	c := sinkConnector(t, slack.NewFake())
	for _, a := range c.Actions() {
		t.Run(a.Name, func(t *testing.T) {
			if !strings.HasPrefix(a.Name, "slack_") {
				t.Errorf("Name = %q, want the connector prefix", a.Name)
			}
			if a.Description == "" {
				t.Error("no description, so a model cannot know when to call it")
			}
			if a.Invoke == nil {
				t.Fatal("no Invoke")
			}
			var schema map[string]any
			if err := json.Unmarshal(a.Schema, &schema); err != nil {
				t.Fatalf("schema is not valid JSON: %v", err)
			}
			if schema["type"] != "object" {
				t.Errorf("schema type = %v, want object", schema["type"])
			}
			if _, ok := schema["properties"]; !ok {
				t.Error("schema has no properties")
			}
		})
	}
}

func TestAnActionCannotReachIntoAnotherWorkspace(t *testing.T) {
	c := sinkConnector(t, slack.NewFake())
	for _, a := range c.Actions() {
		args := json.RawMessage(`{"text":"x","name":"eyes","ts":"1","channel":"C0OTHER"}`)
		if _, err := a.Invoke(context.Background(), address.MustParse("slack://T99OTHER/C1"), args); !isUndeliverable(err) {
			t.Errorf("%s against another workspace: error = %v, want ErrUndeliverable", a.Name, err)
		}
	}
}

// ── Roles ───────────────────────────────────────────────────────────

func TestSlackImplementsAllThreeFaces(t *testing.T) {
	c := sinkConnector(t, slack.NewFake())
	roles := connector.RolesOf(c)
	if !roles.Trigger || !roles.Sink || !roles.Actor {
		t.Errorf("RolesOf(slack) = %+v, want all three faces", roles)
	}
}

func TestNewRejectsAConfigurationThatCouldNotAddress(t *testing.T) {
	tests := []struct {
		name string
		cfg  slack.Config
	}{
		{name: "no api", cfg: slack.Config{}},
		{name: "a team that cannot be a workspace", cfg: slack.Config{API: slack.NewFake(), Team: "T01:BAD"}},
		{name: "a bot mapped to an unusable agent name", cfg: slack.Config{API: slack.NewFake(), Bots: map[string]string{"U1": "docs bot"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := slack.New(tc.cfg); err == nil {
				t.Error("New accepted a configuration that cannot work")
			}
		})
	}
}

// receive waits for an event and checks it renders through the event
// catalog as its request text.
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

// posted returns the message.posted.v1 body of e.
func posted(t *testing.T, e event.Envelope) event.MessagePosted {
	t.Helper()
	b, ok := e.Data.(event.MessagePosted)
	if !ok {
		t.Fatalf("%s carries %T, want message.posted.v1", e.Type, e.Data)
	}
	return b
}
