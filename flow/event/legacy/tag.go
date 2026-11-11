package legacy

import (
	"encoding/json"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/attachment"
	"github.com/urmzd/saige/flow/event"
)

// Trigger is an old tag read as an event, with the two things a tag held
// that an event does not: the agent it named and where to deliver.
type Trigger struct {
	Event event.Envelope
	// Agent is the agent the tag named.
	Agent string
	// Deliver is where the run's events went, as channels and type prefixes.
	Deliver []Route
}

// Route is an old delivery route: a target channel and the type prefixes
// its kind selectors chose.
type Route struct {
	Target string
	Types  []string
}

// oldTag is the JSON shape of the old envelope.Tag.
type oldTag struct {
	ID          string                  `json:"id"`
	Tenant      string                  `json:"tenant"`
	Agent       string                  `json:"agent"`
	Origin      string                  `json:"origin"`
	Source      address.Address         `json:"source"`
	Text        string                  `json:"text"`
	Actor       event.Actor             `json:"actor"`
	Deliver     []oldRoute              `json:"deliver"`
	Meta        map[string]string       `json:"meta"`
	Attachments []attachment.Attachment `json:"attachments"`
	At          time.Time               `json:"at"`
}

type oldRoute struct {
	Target address.Address `json:"target"`
	Kinds  []string        `json:"kinds"`
}

// DecodeTag reads an old tag as an event. Its meta chooses the type:
// cron_* keys make cron.tick.v1, github_* keys a github.* type, jira_* keys
// a jira.* type, a slash command slack.command.invoked.v1, and anything else
// message.posted.v1. Meta keys with a typed field move into the body. Other
// keys stay in the body's Context, and keys starting with "_" move to
// Private.
//
// The body renders exactly the request text the tag rendered. When a typed
// body would not, because the meta is not shaped the way its connector
// builds it, the tag is read as message.posted.v1 with all of its meta in
// Context. An old kind selector this package does not know is an error.
func DecodeTag(raw []byte) (Trigger, error) {
	var t oldTag
	if err := json.Unmarshal(raw, &t); err != nil {
		return Trigger{}, fmt.Errorf("legacy: tag: %w", err)
	}
	public := map[string]string{}
	private := event.Private{}
	for k, v := range t.Meta {
		if strings.HasPrefix(k, "_") {
			private[k] = v
		} else {
			public[k] = v
		}
	}
	req := event.Request{Text: t.Text, Actor: t.Actor, Attachments: t.Attachments}
	if t.Agent != "" {
		req.Mentions = []string{t.Agent}
	}

	body := typed(t.Origin, req, maps.Clone(public))
	want := event.RequestText(t.Text, public)
	if got, err := renderBody(body); err != nil || got != want {
		req.Context = public
		if len(public) == 0 {
			req.Context = nil
		}
		body = event.MessagePosted{Request: req}
	}

	env := event.Envelope{
		ID:      t.ID,
		Source:  sourceOf(t),
		Type:    body.Type(),
		Subject: strings.Join(t.Source.Path, "/"),
		Channel: channelOf(t),
		Time:    t.At,
		Tenant:  t.Tenant,
		Data:    body,
	}
	if len(private) > 0 {
		env.Private = private
	}
	out := Trigger{Event: env, Agent: t.Agent}
	for i, r := range t.Deliver {
		types, err := KindSelectors(r.Kinds)
		if err != nil {
			return Trigger{}, fmt.Errorf("legacy: tag %s route %d: %w", t.ID, i, err)
		}
		out.Deliver = append(out.Deliver, Route{Target: r.Target.String(), Types: types})
	}
	return out, nil
}

func renderBody(b event.Body) (string, error) {
	e, ok := event.Lookup(b.Type())
	if !ok {
		return "", fmt.Errorf("legacy: %s is not registered", b.Type())
	}
	return e.Render(event.Envelope{Type: b.Type(), Data: b})
}

func sourceOf(t oldTag) string {
	scheme := t.Origin
	if scheme == "" {
		scheme = "fiber"
	}
	ws := t.Source.Workspace
	if ws == "" {
		ws = t.Tenant
	}
	if ws == "" {
		ws = "default"
	}
	return scheme + "://" + ws
}

func channelOf(t oldTag) string {
	if !t.Source.IsZero() {
		return t.Source.String()
	}
	return sourceOf(t) + "/" + t.Origin
}

// take removes key from m and returns its value.
func take(m map[string]string, key string) string {
	v := m[key]
	delete(m, key)
	return v
}

// takeInt removes key from m when it holds an integer. A value that is not
// one stays in m, so the render check sees it.
func takeInt(m map[string]string, key string) int64 {
	v, ok := m[key]
	if !ok {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0
	}
	delete(m, key)
	return n
}

func takeList(m map[string]string, key string) []string {
	if v := take(m, key); v != "" {
		return strings.Split(v, ",")
	}
	return nil
}

func withContext(r event.Request, rest map[string]string) event.Request {
	if len(rest) > 0 {
		r.Context = rest
	}
	return r
}

// typed builds the body the tag's meta names. m is consumed.
func typed(origin string, r event.Request, m map[string]string) event.Body {
	switch {
	case origin == "cron" || m["cron_schedule"] != "":
		return cronTick(r, m)
	case m["github_event"] != "":
		return gitHub(r, m)
	case m["jira_event"] != "":
		return jira(r, m)
	case m["slack_event"] == "slash_command":
		b := event.SlackCommandInvoked{}
		take(m, "slack_event")
		b.Team = take(m, "slack_team")
		b.Channel = take(m, "slack_channel")
		b.User = take(m, "slack_user")
		b.Command = take(m, "slack_command")
		b.AppID = take(m, "slack_app_id")
		b.Request = withContext(r, m)
		return b
	case m["slack_event"] != "":
		s := &event.SlackRef{
			Team:        take(m, "slack_team"),
			Channel:     take(m, "slack_channel"),
			User:        take(m, "slack_user"),
			Event:       take(m, "slack_event"),
			TS:          take(m, "slack_ts"),
			ThreadTS:    take(m, "slack_thread_ts"),
			ChannelType: take(m, "slack_channel_type"),
			AppID:       take(m, "slack_app_id"),
		}
		return event.MessagePosted{Request: withContext(r, m), Slack: s}
	}
	return event.MessagePosted{Request: withContext(r, m)}
}

func cronTick(r event.Request, m map[string]string) event.Body {
	b := event.CronTick{Schedule: take(m, "cron_schedule"), Expression: take(m, "cron_expression")}
	if at, err := time.Parse(event.CronOccurrenceFormat, m["cron_occurrence"]); err == nil {
		b.Occurrence = at
		delete(m, "cron_occurrence")
	}
	b.Request = withContext(r, m)
	return b
}

func gitHub(r event.Request, m map[string]string) event.Body {
	typ, ok := event.GitHubType(m["github_event"], m["github_action"])
	if !ok {
		return event.MessagePosted{Request: withContext(r, m)}
	}
	delete(m, "github_event")
	delete(m, "github_action")
	g := event.GitHubRef{
		Owner:   take(m, "github_owner"),
		Repo:    take(m, "github_repo"),
		Number:  int(takeInt(m, "github_number")),
		Pull:    take(m, "github_kind") == "pull",
		Title:   take(m, "github_title"),
		Sender:  take(m, "github_sender"),
		State:   take(m, "github_state"),
		Author:  take(m, "github_author"),
		URL:     take(m, "github_url"),
		BaseRef: take(m, "github_base_ref"),
		HeadRef: take(m, "github_head_ref"),
		HeadSHA: take(m, "github_head_sha"),
		Labels:  takeList(m, "github_labels"),
		Draft:   take(m, "github_draft") == "true",
	}
	g.InstallationID = takeInt(m, "github_installation_id")
	comment := takeInt(m, "github_comment_id")
	root := takeInt(m, "github_review_root_id")
	req := withContext(r, m)
	switch typ {
	case event.TypeGitHubIssueCommentCreated:
		return event.GitHubIssueCommentCreated{Request: req, GitHubRef: g, CommentID: comment}
	case event.TypeGitHubReviewCommentCreated:
		return event.GitHubReviewCommentCreated{Request: req, GitHubRef: g, CommentID: comment, ReviewRootID: root}
	case event.TypeGitHubReviewSubmitted:
		return event.GitHubReviewSubmitted{Request: req, GitHubRef: g, CommentID: comment}
	case event.TypeGitHubIssueOpened:
		return event.GitHubIssueOpened{Request: req, GitHubRef: g}
	case event.TypeGitHubIssueLabeled:
		return event.GitHubIssueLabeled{Request: req, GitHubRef: g}
	case event.TypeGitHubPullOpened:
		return event.GitHubPullOpened{Request: req, GitHubRef: g}
	case event.TypeGitHubPullReadyForReview:
		return event.GitHubPullReadyForReview{Request: req, GitHubRef: g}
	case event.TypeGitHubReviewRequested:
		return event.GitHubReviewRequested{Request: req, GitHubRef: g}
	case event.TypeGitHubPullSynchronize:
		return event.GitHubPullSynchronize{Request: req, GitHubRef: g}
	default:
		return event.GitHubPullLabeled{Request: req, GitHubRef: g}
	}
}

func jira(r event.Request, m map[string]string) event.Body {
	assigned := m["jira_assigned"] == "true"
	comment, hasComment := m["jira_comment_id"]
	created := m["jira_event"] == "jira:issue_created"
	if !assigned && !hasComment && !created {
		return event.MessagePosted{Request: withContext(r, m)}
	}
	j := event.JiraRef{
		WebhookEvent: take(m, "jira_event"),
		Site:         take(m, "jira_site"),
		Key:          take(m, "jira_key"),
		IssueEvent:   take(m, "jira_issue_event"),
		ID:           take(m, "jira_id"),
		Summary:      take(m, "jira_summary"),
		Status:       take(m, "jira_status"),
		IssueType:    take(m, "jira_type"),
		Priority:     take(m, "jira_priority"),
		Project:      take(m, "jira_project"),
		ProjectName:  take(m, "jira_project_name"),
		Assignee:     take(m, "jira_assignee"),
		Reporter:     take(m, "jira_reporter"),
		Labels:       takeList(m, "jira_labels"),
		URL:          take(m, "jira_url"),
	}
	switch {
	case assigned:
		delete(m, "jira_assigned")
		return event.JiraIssueAssigned{Request: withContext(r, m), JiraRef: j}
	case hasComment:
		delete(m, "jira_comment_id")
		return event.JiraCommentCreated{Request: withContext(r, m), JiraRef: j, CommentID: comment}
	}
	return event.JiraIssueCreated{Request: withContext(r, m), JiraRef: j}
}
