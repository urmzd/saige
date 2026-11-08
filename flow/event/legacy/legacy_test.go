package legacy_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/event/legacy"
)

var at = time.Date(2026, 10, 28, 14, 3, 11, 0, time.UTC)

// kind is a v0.25 envelope.Kind. The package is gone, so its constants are
// pinned here: every kind stored data can still carry.
type kind string

// family is the kind's leading component, as envelope.Kind.Family was.
func (k kind) family() string {
	if i := strings.IndexByte(string(k), '.'); i >= 0 {
		return string(k[:i])
	}
	return string(k)
}

// matches is envelope.Kind.Matches: the exact kind or its family.
func (k kind) matches(sel string) bool {
	return sel == "" || sel == string(k) || sel == k.family()
}

const (
	kindText         kind = "delta.text"
	kindThinking     kind = "delta.thinking"
	kindToolCall     kind = "delta.tool.call"
	kindToolDone     kind = "delta.tool.done"
	kindCitation     kind = "delta.citation"
	kindAccepted     kind = "lifecycle.accepted"
	kindStarted      kind = "lifecycle.started"
	kindParked       kind = "lifecycle.parked"
	kindResumed      kind = "lifecycle.resumed"
	kindCompleted    kind = "lifecycle.completed"
	kindFailed       kind = "lifecycle.failed"
	kindChildParked  kind = "lifecycle.child.parked"
	kindStopped      kind = "lifecycle.stopped"
	kindActionTaken  kind = "action.taken"
	kindUsageCall    kind = "usage.call"
	kindUsageChild   kind = "usage.child"
	kindCompacted    kind = "context.compacted"
	kindAgentCreated kind = "control.created"
	kindAgentRevised kind = "control.revised"
	kindAgentDeleted kind = "control.deleted"
	kindStop         kind = "control.stop"
	kindCompact      kind = "control.compact"
)

// oldKinds is every v0.25 kind by its constant's name.
func oldKinds(*testing.T) map[string]string {
	out := map[string]string{}
	for _, k := range []kind{
		kindText, kindThinking, kindToolCall, kindToolDone, kindCitation,
		kindAccepted, kindStarted, kindParked, kindResumed, kindCompleted, kindFailed, kindChildParked, kindStopped,
		kindActionTaken, kindUsageCall, kindUsageChild, kindCompacted,
		kindAgentCreated, kindAgentRevised, kindAgentDeleted, kindStop, kindCompact,
	} {
		out[string(k)] = string(k)
	}
	return out
}

func TestEveryOldKindMaps(t *testing.T) {
	kinds := oldKinds(t)
	var dropped []string
	for name, kind := range kinds {
		typ, err := legacy.TypeOf(kind)
		switch {
		case errors.Is(err, legacy.ErrDropped):
			dropped = append(dropped, kind)
		case err != nil:
			t.Errorf("%s (%q) maps to nothing: %v", name, kind, err)
		default:
			if _, ok := event.Lookup(typ); !ok {
				t.Errorf("%s maps to %q, which is not registered", name, typ)
			}
		}
	}
	slices.Sort(dropped)
	if want := []string{"control.created", "control.deleted", "control.revised"}; !slices.Equal(dropped, want) {
		t.Errorf("dropped = %v, want %v", dropped, want)
	}
	if got := legacy.DroppedKinds(); !slices.Equal(got, dropped) {
		t.Errorf("DroppedKinds() = %v, want %v", got, dropped)
	}
	// No mapping names a kind that does not exist.
	values := slices.Collect(func(yield func(string) bool) {
		for _, v := range kinds {
			if !yield(v) {
				return
			}
		}
	})
	for _, k := range legacy.KeptKinds() {
		if !slices.Contains(values, k) {
			t.Errorf("KeptKinds has %q, which is no v0.25 kind", k)
		}
	}
	if len(legacy.KeptKinds())+len(dropped) != len(kinds) {
		t.Errorf("%d kept and %d dropped of %d kinds", len(legacy.KeptKinds()), len(dropped), len(kinds))
	}
	if _, err := legacy.TypeOf("delta.future"); !errors.Is(err, legacy.ErrUnknown) {
		t.Errorf("TypeOf(unknown) = %v, want ErrUnknown", err)
	}
}

// Each old selector selects, through its prefixes, exactly the types its old
// kinds map to, apart from the dropped ones.
func TestKindSelectorSelectsWhatItDid(t *testing.T) {
	kinds := oldKinds(t)
	selectors := map[string]bool{}
	for _, k := range kinds {
		selectors[k] = true
		selectors[kind(k).family()] = true
	}
	for sel := range selectors {
		prefixes, err := legacy.KindSelector(sel)
		if err != nil {
			if errors.Is(err, legacy.ErrDropped) {
				continue
			}
			t.Errorf("KindSelector(%q): %v", sel, err)
			continue
		}
		for _, k := range kinds {
			typ, err := legacy.TypeOf(k)
			if err != nil {
				continue
			}
			was := kind(k).matches(sel)
			now := slices.ContainsFunc(prefixes, func(p string) bool { return event.Selects(p, typ) })
			if was != now {
				t.Errorf("selector %q on %q: was %v, now %v (prefixes %v)", sel, k, was, now, prefixes)
			}
		}
	}
	for _, bad := range []string{"", "lifecycle.done", "deltas", "control.created"} {
		if _, err := legacy.KindSelector(bad); err == nil {
			t.Errorf("KindSelector(%q) is not an error", bad)
		}
	}
	// An empty route selected every kind that is not opt-in.
	all, err := legacy.KindSelectors(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range kinds {
		typ, err := legacy.TypeOf(k)
		if err != nil || event.Family(typ) != "run" {
			continue
		}
		was := event.Selects("run", typ)
		now := slices.ContainsFunc(all, func(p string) bool { return event.Selects(p, typ) })
		if was != now {
			t.Errorf("empty route on %q: was %v, now %v", k, was, now)
		}
	}
}

// oldEvent is the JSON of a v0.25 envelope.Event.
type oldEvent struct {
	Seq     uint64    `json:"seq"`
	Topic   string    `json:"topic"`
	RunID   string    `json:"run_id"`
	Tenant  string    `json:"tenant"`
	Agent   string    `json:"agent"`
	Rev     int       `json:"rev"`
	Origin  string    `json:"origin"`
	Kind    kind      `json:"kind"`
	Payload []byte    `json:"payload,omitempty"`
	At      time.Time `json:"at"`
}

// wireEvent is the v0.25 bus encoding, as pkg/bus wrote it: the event with
// its sequence left to the row.
type wireEvent struct {
	oldEvent
	Seq uint64 `json:"seq,omitempty"`
}

// oldLifecycle is the v0.25 payload of every lifecycle kind but stopped.
type oldLifecycle struct {
	Attempt  int             `json:"attempt,omitempty"`
	Rev      int             `json:"rev,omitempty"`
	Error    string          `json:"error,omitempty"`
	Terminal bool            `json:"terminal,omitempty"`
	Reason   string          `json:"reason,omitempty"`
	Text     string          `json:"text,omitempty"`
	Usage    *event.RunUsage `json:"usage,omitempty"`
	Question *event.Question `json:"question,omitempty"`
	Answer   *event.Answer   `json:"answer,omitempty"`
}

func newOldEvent(t *testing.T, k kind, body any) oldEvent {
	t.Helper()
	p, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return oldEvent{
		Seq: 7, Topic: "agent:helper:r1", RunID: "r1", Tenant: "acme", Agent: "helper", Rev: 3,
		Origin: "slack", Kind: k, Payload: p, At: at,
	}
}

type oldCase struct {
	kind kind
	in   any
	want event.Body
}

func oldCases() []oldCase {
	q := event.Question{ID: "q1", Kind: "approval", Prompt: "May I?", Options: []string{"yes", "no"}}
	usage := &event.RunUsage{Own: event.Usage{PromptTokens: 9, Cost: &event.Cost{Micros: 12, Currency: "USD"}}}
	wantUsage := &event.RunUsage{Own: event.Usage{PromptTokens: 9, Cost: &event.Cost{Micros: 12, Currency: "USD"}}}
	// A lifecycle payload with every field set: each type keeps its own.
	full := oldLifecycle{
		Attempt: 2, Rev: 3, Error: "boom", Terminal: true, Reason: "waiting", Text: "done", Usage: usage,
		Question: &q, Answer: &event.Answer{QuestionID: "q1", Decision: "approve"},
	}
	wq := event.Question{ID: "q1", Kind: "approval", Prompt: "May I?", Options: []string{"yes", "no"}}
	return []oldCase{
		{kindAccepted, full, event.Accepted{Attempt: 2}},
		{kindStarted, full, event.Started{Attempt: 2}},
		{kindParked, full, event.Parked{Attempt: 2, Reason: "waiting", Question: &wq}},
		{kindResumed, full, event.Resumed{Attempt: 2, Answer: &event.Answer{QuestionID: "q1", Decision: "approve"}}},
		{kindCompleted, full, event.Completed{Attempt: 2, Text: "done", Usage: wantUsage}},
		{kindFailed, full, event.Failed{Attempt: 2, Error: "boom", Terminal: true}},
		{kindStopped, event.Stopped{Attempt: 1, Reason: "enough", By: "ana", Text: "so far"}, event.Stopped{Attempt: 1, Reason: "enough", By: "ana", Text: "so far"}},
		{kindChildParked, event.ChildParked{RunID: "r2", Agent: "deployer", Question: q}, event.ChildParked{RunID: "r2", Agent: "deployer", Question: wq}},
		{kindText, event.Text{Text: "Hello"}, event.Text{Text: "Hello"}},
		{kindThinking, event.Thinking{Text: "Hmm"}, event.Thinking{Text: "Hmm"}},
		{kindToolCall, event.ToolCall{ID: "c1", Name: "search", Phase: "executing", Arguments: json.RawMessage(`{"q":"x"}`)},
			event.ToolCall{ID: "c1", Name: "search", Phase: "executing", Arguments: json.RawMessage(`{"q":"x"}`)}},
		{kindToolDone, event.ToolDone{ID: "c1", Name: "search", Result: "3 hits"}, event.ToolDone{ID: "c1", Name: "search", Result: "3 hits"}},
		{kindCitation, event.Citation{Label: "[1]", Text: "q", Source: event.Provenance{SourceURI: "https://x"}},
			event.Citation{Label: "[1]", Text: "q", Source: event.Provenance{SourceURI: "https://x"}}},
		{kindActionTaken, event.Action{Name: "jira_comment", Target: "jira://acme/P-1", Summary: "ok"},
			event.Action{Name: "jira_comment", Target: "jira://acme/P-1", Summary: "ok"}},
		{kindUsageCall, event.UsageCall{AccountingID: "r1/a1/1", Model: "m", PromptTokens: 9},
			event.UsageCall{AccountingID: "r1/a1/1", Model: "m", PromptTokens: 9}},
		{kindUsageChild, event.UsageChild{AccountingID: "run:r2", RunID: "r2", Usage: event.Usage{CompletionTokens: 4}},
			event.UsageChild{AccountingID: "run:r2", RunID: "r2", Usage: event.Usage{CompletionTokens: 4}}},
		{kindCompacted, map[string]any{"trigger": "input_pressure", "summarized": 3, "kept": 1},
			event.Compaction{Trigger: "input_pressure", Summarized: 3, Kept: 1}},
		{kindStop, event.StopRequest{RunID: "r1", Reason: "stop", By: "ana", At: at}, event.StopRequest{RunID: "r1", Reason: "stop", By: "ana", At: at}},
		{kindCompact, event.CompactRequest{RunID: "r1", By: "ana", At: at}, event.CompactRequest{RunID: "r1", By: "ana", At: at}},
	}
}

func checkDecoded(t *testing.T, c oldCase, got event.Envelope) {
	t.Helper()
	want, _ := legacy.TypeOf(string(c.kind))
	if got.Type != want || got.Run != "r1" || got.Seq != 7 || got.Tenant != "acme" || got.Agent != "helper" ||
		got.Version != "3" || !got.Time.Equal(at) || got.ID != "r1:7" || got.Channel != legacy.RunChannel("acme", "helper", "r1") {
		t.Errorf("%s: envelope = %+v", c.kind, got)
	}
	if !reflect.DeepEqual(got.Data, c.want) {
		t.Errorf("%s: data = %#v, want %#v", c.kind, got.Data, c.want)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("%s: Validate: %v", c.kind, err)
	}
}

func TestDecodeBusRow(t *testing.T) {
	for _, c := range oldCases() {
		body, err := json.Marshal(wireEvent{oldEvent: newOldEvent(t, c.kind, c.in)})
		if err != nil {
			t.Fatal(err)
		}
		got, err := legacy.DecodeBusRow(7, body)
		if err != nil {
			t.Fatalf("%s: %v", c.kind, err)
		}
		checkDecoded(t, c, got)
	}
	for k, want := range map[kind]error{
		kindAgentCreated: legacy.ErrDropped,
		"delta.future":   legacy.ErrUnknown,
	} {
		body, _ := json.Marshal(wireEvent{oldEvent: newOldEvent(t, k, map[string]any{})})
		if _, err := legacy.DecodeBusRow(7, body); !errors.Is(err, want) {
			t.Errorf("%s: err = %v, want %v", k, err, want)
		}
	}
	body, _ := json.Marshal(wireEvent{oldEvent: newOldEvent(t, kindText, map[string]any{"text": 5})})
	if _, err := legacy.DecodeBusRow(7, body); err == nil {
		t.Error("a broken known payload decoded")
	}
}

// oldEntry is a journal entry as the runtime wrote it before the union.
type oldEntry struct {
	Seq     uint64          `json:"seq"`
	Topic   string          `json:"topic"`
	RunID   string          `json:"run_id"`
	Tenant  string          `json:"tenant,omitempty"`
	Agent   string          `json:"agent"`
	Rev     int             `json:"rev"`
	Origin  string          `json:"origin,omitempty"`
	Kind    kind            `json:"kind"`
	Payload json.RawMessage `json:"payload,omitempty"`
	At      time.Time       `json:"at"`
}

func oldEntryOf(e oldEvent) oldEntry {
	return oldEntry{
		Seq: e.Seq, Topic: e.Topic, RunID: e.RunID, Tenant: e.Tenant,
		Agent: e.Agent, Rev: e.Rev, Origin: e.Origin, Kind: e.Kind,
		Payload: json.RawMessage(e.Payload), At: e.At,
	}
}

func TestDecodeEntry(t *testing.T) {
	for _, c := range oldCases() {
		raw, err := json.Marshal(oldEntryOf(newOldEvent(t, c.kind, c.in)))
		if err != nil {
			t.Fatal(err)
		}
		got, err := legacy.DecodeEntry(raw)
		if err != nil {
			t.Fatalf("%s: %v", c.kind, err)
		}
		checkDecoded(t, c, got)
	}
}

// A store that marshaled the old event whole wrote its []byte payload as
// base64.
func TestDecodeEntryBase64(t *testing.T) {
	for _, c := range oldCases() {
		raw, err := json.Marshal(newOldEvent(t, c.kind, c.in))
		if err != nil {
			t.Fatal(err)
		}
		got, err := legacy.DecodeEntry(raw)
		if err != nil {
			t.Fatalf("%s: %v", c.kind, err)
		}
		checkDecoded(t, c, got)
	}
	raw := []byte(`{"seq":1,"run_id":"r1","agent":"a","kind":"delta.text","payload":"not base64!"}`)
	if _, err := legacy.DecodeEntry(raw); err == nil {
		t.Error("a broken base64 payload decoded")
	}
}

// oldTag is the JSON shape of the removed envelope.Tag, as it was stored.
type oldTag struct {
	ID      string            `json:"id"`
	Tenant  string            `json:"tenant,omitempty"`
	Agent   string            `json:"agent,omitempty"`
	Origin  string            `json:"origin"`
	Source  address.Address   `json:"source,omitzero"`
	Text    string            `json:"text"`
	Deliver []oldRoute        `json:"deliver,omitempty"`
	Meta    map[string]string `json:"meta,omitempty"`
	At      time.Time         `json:"at"`
}

type oldRoute struct {
	Target address.Address `json:"target"`
	Kinds  []string        `json:"kinds,omitempty"`
}

func decodeTag(t *testing.T, tag oldTag) legacy.Trigger {
	t.Helper()
	raw, err := json.Marshal(tag)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := legacy.DecodeTag(raw)
	if err != nil {
		t.Fatalf("DecodeTag: %v", err)
	}
	e, ok := event.Lookup(tr.Event.Type)
	if !ok {
		t.Fatalf("%s is not registered", tr.Event.Type)
	}
	got, err := e.Render(tr.Event)
	if err != nil {
		t.Fatal(err)
	}
	if want := event.RequestText(tag.Text, tag.Meta); got != want {
		t.Errorf("%s renders\n%q, want\n%q", tr.Event.Type, got, want)
	}
	return tr
}

func TestDecodeTag(t *testing.T) {
	slackSource := address.Address{Connector: "slack", Workspace: "T01", Path: []string{"C02"}}.WithParam("thread", "1699.001")
	for _, tc := range []struct {
		name string
		tag  oldTag
		want string
	}{
		{"slack mention", oldTag{ID: "Ev1", Origin: "slack", Agent: "helper", Source: slackSource, Text: "hi",
			Meta: map[string]string{"slack_team": "T01", "slack_channel": "C02", "slack_user": "U1", "slack_event": "app_mention", "slack_ts": "1699.002", "slack_thread_ts": "1699.001"}},
			event.TypeMessagePosted},
		{"slash command", oldTag{ID: "command:1", Origin: "slack", Text: "/ask x",
			Meta: map[string]string{"slack_team": "T01", "slack_channel": "C02", "slack_user": "U1", "slack_event": "slash_command", "slack_command": "/ask"}},
			event.TypeSlackCommandInvoked},
		{"console", oldTag{ID: "c1", Origin: "console", Agent: "helper", Text: "hi", Meta: map[string]string{"humagen.project": "p"}},
			event.TypeMessagePosted},
		{"github review comment", oldTag{ID: "d1", Origin: "github", Text: "look",
			Meta: map[string]string{"github_event": "pull_request_review_comment", "github_action": "created", "github_owner": "o", "github_repo": "r",
				"github_number": "4", "github_kind": "pull", "github_title": "T", "github_sender": "s", "github_comment_id": "9", "github_review_root_id": "8",
				"github_labels": "a,b", "github_draft": "true", "github_installation_id": "77"}},
			event.TypeGitHubReviewCommentCreated},
		{"github with a malformed number", oldTag{ID: "d2", Origin: "github", Text: "look",
			Meta: map[string]string{"github_event": "issues", "github_action": "opened", "github_owner": "o", "github_repo": "r",
				"github_number": "four", "github_kind": "issue", "github_title": "T", "github_sender": "s"}},
			event.TypeMessagePosted},
		{"github event without a type", oldTag{ID: "d3", Origin: "github", Text: "x", Meta: map[string]string{"github_event": "push", "github_action": ""}},
			event.TypeMessagePosted},
		{"jira assigned", oldTag{ID: "j1", Origin: "jira", Text: "ticket",
			Meta: map[string]string{"jira_event": "jira:issue_updated", "jira_site": "acme", "jira_key": "P-1", "jira_assigned": "true", "jira_labels": "x"}},
			event.TypeJiraIssueAssigned},
		{"jira comment", oldTag{ID: "j2", Origin: "jira", Text: "please",
			Meta: map[string]string{"jira_event": "comment_created", "jira_site": "acme", "jira_key": "P-1", "jira_comment_id": "100"}},
			event.TypeJiraCommentCreated},
		{"jira created", oldTag{ID: "j3", Origin: "jira", Text: "new",
			Meta: map[string]string{"jira_event": "jira:issue_created", "jira_site": "acme", "jira_key": "P-1"}},
			event.TypeJiraIssueCreated},
		{"cron", oldTag{ID: "nightly@1", Origin: "cron", Tenant: "acme", Agent: "helper", Text: "report",
			Source: address.Address{Connector: "cron", Workspace: "acme", Path: []string{"nightly"}},
			Meta:   map[string]string{"cron_schedule": "nightly", "cron_expression": "0 3 * * *", "cron_occurrence": "2026-10-28T03:00:00Z", "team": "ops"}},
			event.TypeCronTick},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.tag.At = at
			tr := decodeTag(t, tc.tag)
			if tr.Event.Type != tc.want {
				t.Errorf("type = %s, want %s", tr.Event.Type, tc.want)
			}
			if tr.Event.ID != tc.tag.ID || tr.Agent != tc.tag.Agent || !tr.Event.Time.Equal(at) || tr.Event.Tenant != tc.tag.Tenant {
				t.Errorf("trigger = %+v", tr)
			}
			if !tc.tag.Source.IsZero() && tr.Event.Channel != tc.tag.Source.String() {
				t.Errorf("channel = %q, want %q", tr.Event.Channel, tc.tag.Source.String())
			}
			if tr.Event.Channel == "" || tr.Event.Source == "" {
				t.Errorf("no channel or source: %+v", tr.Event)
			}
		})
	}
}

func TestDecodeTagMovesPrivateMetaAndRoutes(t *testing.T) {
	target := address.Address{Connector: "webhook", Workspace: "acme", Path: []string{"hook"}}
	tag := oldTag{
		ID: "c1", Origin: "console", Agent: "helper", Text: "hi", At: at,
		Meta:    map[string]string{"_host.sources": "playbook", "channel": "ops"},
		Deliver: []oldRoute{{Target: target, Kinds: []string{"lifecycle.completed", "usage"}}, {Target: target}},
	}
	tr := decodeTag(t, tag)
	if tr.Event.Private["_host.sources"] != "playbook" || len(tr.Event.Private) != 1 {
		t.Errorf("Private = %v", tr.Event.Private)
	}
	b := tr.Event.Data.(event.MessagePosted)
	if !reflect.DeepEqual(b.Context, map[string]string{"channel": "ops"}) || !slices.Equal(b.Mentions, []string{"helper"}) {
		t.Errorf("body = %+v", b)
	}
	want := []legacy.Route{
		{Target: target.String(), Types: []string{event.TypeCompleted, "run.usage"}},
		{Target: target.String(), Types: []string{"run"}},
	}
	if !reflect.DeepEqual(tr.Deliver, want) {
		t.Errorf("Deliver = %+v, want %+v", tr.Deliver, want)
	}
	raw, _ := json.Marshal(tr.Event)
	if bytes.Contains(raw, []byte("playbook")) || bytes.Contains(raw, []byte("_host")) {
		t.Errorf("private meta serialized: %s", raw)
	}

	tag.Deliver = []oldRoute{{Target: target, Kinds: []string{"lifecycle.finished"}}}
	raw, _ = json.Marshal(tag)
	if _, err := legacy.DecodeTag(raw); !errors.Is(err, legacy.ErrUnknown) {
		t.Errorf("an unknown selector: err = %v, want ErrUnknown", err)
	}
}
