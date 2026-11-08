package event

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/urmzd/saige/flow/attachment"
)

// Ingress types: what a surface publishes when something happens on it.
const (
	TypeMessagePosted       = "message.posted.v1"
	TypeSlackCommandInvoked = "slack.command.invoked.v1"
	TypeSlackSessionStopped = "slack.session.stopped.v1"

	TypeGitHubIssueCommentCreated  = "github.issue_comment.created.v1"
	TypeGitHubReviewCommentCreated = "github.pull_request_review_comment.created.v1"
	TypeGitHubReviewSubmitted      = "github.pull_request_review.submitted.v1"
	TypeGitHubIssueOpened          = "github.issues.opened.v1"
	TypeGitHubIssueLabeled         = "github.issues.labeled.v1"
	TypeGitHubPullOpened           = "github.pull_request.opened.v1"
	TypeGitHubPullReadyForReview   = "github.pull_request.ready_for_review.v1"
	TypeGitHubReviewRequested      = "github.pull_request.review_requested.v1"
	TypeGitHubPullSynchronize      = "github.pull_request.synchronize.v1"
	TypeGitHubPullLabeled          = "github.pull_request.labeled.v1"

	TypeJiraCommentCreated = "jira.comment.created.v1"
	TypeJiraIssueCreated   = "jira.issue.created.v1"
	TypeJiraIssueAssigned  = "jira.issue.assigned.v1"

	TypeCronTick = "cron.tick.v1"
)

// Request is what an ingress event asks of an agent. Every ingress body that
// can start a run embeds it.
type Request struct {
	// Text is the request, with any agent mention already taken out.
	Text string `json:"text"`
	// Actor raised the event.
	Actor Actor `json:"actor,omitzero"`
	// Mentions are the agents the request names, in order.
	Mentions []string `json:"mentions,omitempty"`
	// References are the agents, people, places and links the request refers
	// to, in the order each was first written. Mentions stays the list of
	// agents that wakes members; References is what a host resolves people
	// and sessions from, and what the model is shown next to the text.
	References []Reference `json:"references,omitempty"`
	// Attachments are the files that arrived with the request.
	Attachments []attachment.Attachment `json:"attachments,omitempty"`
	// Context is host context shown to the model next to the body's own
	// fields. It never overrides a typed field. A key starting with "_" is
	// never rendered: private host data belongs in Envelope.Private.
	Context map[string]string `json:"context,omitempty"`
}

func (r Request) request() Request { return r }

// Kinds of reference (Reference.Kind).
const (
	ReferenceAgent  = "agent"
	ReferencePerson = "person"
	ReferencePlace  = "place"
	ReferenceLink   = "link"
)

// Reference is one thing a request refers to: an agent or a person it names,
// a place it points at, or a link it carries. It is one routing language for
// every surface: a host maps a person's ID to its own people and an Address
// to its sessions and places without knowing how the surface wrote them.
type Reference struct {
	// Kind is ReferenceAgent, ReferencePerson, ReferencePlace or
	// ReferenceLink. A link is a URL the text carried; a place is one it
	// named without a URL, such as GitHub's "#12" or a Slack channel.
	Kind string `json:"kind"`
	// Name is the agent's name for an agent, and otherwise what a person saw
	// in the text: "@ada", "#general", "#12", a link's label. Empty when they
	// saw only an id, or the URL itself.
	Name string `json:"name,omitempty"`
	// ID is the native id on the surface the event came from (the scheme of
	// its source): a Slack user or channel id, a GitHub login, a Jira account
	// id.
	ID string `json:"id,omitempty"`
	// Address is the canonical address of what is referred to, when it is
	// known: "slack://T01/C02", "slack://T01/C02?thread=1699.001",
	// "github://acme/web/pull/12", or what the host's link reader returned,
	// such as a session id.
	Address string `json:"address,omitempty"`
	// URL is the link exactly as it was written, line ranges and all.
	URL string `json:"url,omitempty"`
}

// message is what a model reads of the request: its text, then the people,
// places and links it refers to. Agents are left out: their mentions were
// taken out of the text on purpose, and an agent reading its own name answers
// as if spoken about.
func (r Request) message() string {
	var lines []string
	for _, ref := range r.References {
		if ref.Kind != ReferenceAgent {
			lines = append(lines, ref.line())
		}
	}
	if len(lines) == 0 {
		return r.Text
	}
	return r.Text + "\n\nReferences:\n" + strings.Join(lines, "\n")
}

// line renders a reference as "- <kind> <name>: <where>", where is a link's
// URL and address, a place's address, or a person's id. A raw id stands
// alone only when nothing names it.
func (ref Reference) line() string {
	name, id := oneLine(ref.Name), ref.ID
	if name == "" && ref.Kind != ReferenceLink {
		name, id = id, ""
	}
	if strings.EqualFold(strings.TrimLeft(name, "@#"), id) {
		id = ""
	}
	where := id
	switch {
	case ref.URL != "" && ref.Address != "":
		where = ref.URL + " (" + ref.Address + ")"
	case ref.URL != "":
		where = ref.URL
	case ref.Address != "":
		where = ref.Address
	}
	s := "- " + ref.Kind
	if name != "" {
		s += " " + name
	}
	if where != "" {
		s += ": " + where
	}
	return oneLine(s)
}

// oneLine keeps a label on its line, so a label cannot forge a reference.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// RequestOf returns the request an ingress body carries, and false for a
// body that carries none.
func RequestOf(b Body) (Request, bool) {
	r, ok := b.(interface{ request() Request })
	if !ok {
		return Request{}, false
	}
	return r.request(), true
}

// SlackRef is where in Slack a message was posted.
type SlackRef struct {
	Team        string `json:"team"`
	Channel     string `json:"channel"`
	User        string `json:"user"`
	Event       string `json:"event"`
	TS          string `json:"ts"`
	ThreadTS    string `json:"thread_ts,omitempty"`
	ChannelType string `json:"channel_type,omitempty"`
	AppID       string `json:"app_id,omitempty"`
}

// MessagePosted is the body of message.posted.v1: a person writing to an
// agent on any surface. Slack is set when the surface is Slack.
type MessagePosted struct {
	Request
	Slack *SlackRef `json:"slack,omitempty"`
}

// SlackCommandInvoked is the body of slack.command.invoked.v1: a slash
// command. Text is the command and its arguments.
type SlackCommandInvoked struct {
	Request
	Command string `json:"command"`
	Team    string `json:"team"`
	Channel string `json:"channel"`
	User    string `json:"user"`
	AppID   string `json:"app_id,omitempty"`
}

// SlackSessionStopped is the body of slack.session.stopped.v1: a person
// pressing stop on an agent session. ThreadTS is empty for a code channel.
type SlackSessionStopped struct {
	Team     string `json:"team"`
	Channel  string `json:"channel"`
	ThreadTS string `json:"thread_ts,omitempty"`
	User     string `json:"user"`
}

// GitHubRef is the issue or pull request a GitHub event is about.
type GitHubRef struct {
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	// Pull reports a pull request rather than an issue.
	Pull           bool     `json:"pull,omitempty"`
	Title          string   `json:"title"`
	Sender         string   `json:"sender"`
	State          string   `json:"state,omitempty"`
	Author         string   `json:"author,omitempty"`
	URL            string   `json:"url,omitempty"`
	BaseRef        string   `json:"base_ref,omitempty"`
	HeadRef        string   `json:"head_ref,omitempty"`
	HeadSHA        string   `json:"head_sha,omitempty"`
	Labels         []string `json:"labels,omitempty"`
	Draft          bool     `json:"draft,omitempty"`
	InstallationID int64    `json:"installation_id,omitempty"`
}

// GitHubIssueCommentCreated is the body of github.issue_comment.created.v1.
type GitHubIssueCommentCreated struct {
	Request
	GitHubRef
	CommentID int64 `json:"comment_id,omitempty"`
}

// GitHubReviewCommentCreated is the body of
// github.pull_request_review_comment.created.v1. ReviewRootID is the first
// comment of the review thread.
type GitHubReviewCommentCreated struct {
	Request
	GitHubRef
	CommentID    int64 `json:"comment_id,omitempty"`
	ReviewRootID int64 `json:"review_root_id,omitempty"`
}

// GitHubReviewSubmitted is the body of github.pull_request_review.submitted.v1.
// CommentID is the review's id.
type GitHubReviewSubmitted struct {
	Request
	GitHubRef
	CommentID int64 `json:"comment_id,omitempty"`
}

// GitHubIssueOpened is the body of github.issues.opened.v1.
type GitHubIssueOpened struct {
	Request
	GitHubRef
}

// GitHubIssueLabeled is the body of github.issues.labeled.v1.
type GitHubIssueLabeled struct {
	Request
	GitHubRef
}

// GitHubPullOpened is the body of github.pull_request.opened.v1.
type GitHubPullOpened struct {
	Request
	GitHubRef
}

// GitHubPullReadyForReview is the body of
// github.pull_request.ready_for_review.v1.
type GitHubPullReadyForReview struct {
	Request
	GitHubRef
}

// GitHubReviewRequested is the body of github.pull_request.review_requested.v1.
// Reviewer is the agent the review was requested from, when it is one.
type GitHubReviewRequested struct {
	Request
	GitHubRef
	Reviewer string `json:"reviewer,omitempty"`
}

// GitHubPullSynchronize is the body of github.pull_request.synchronize.v1.
type GitHubPullSynchronize struct {
	Request
	GitHubRef
}

// GitHubPullLabeled is the body of github.pull_request.labeled.v1.
type GitHubPullLabeled struct {
	Request
	GitHubRef
}

// JiraRef is the Jira issue an event is about. WebhookEvent and IssueEvent
// are Jira's own names for what happened.
type JiraRef struct {
	Site         string   `json:"site"`
	Key          string   `json:"key"`
	WebhookEvent string   `json:"webhook_event"`
	IssueEvent   string   `json:"issue_event,omitempty"`
	ID           string   `json:"id,omitempty"`
	Summary      string   `json:"summary,omitempty"`
	Status       string   `json:"status,omitempty"`
	IssueType    string   `json:"issue_type,omitempty"`
	Priority     string   `json:"priority,omitempty"`
	Project      string   `json:"project,omitempty"`
	ProjectName  string   `json:"project_name,omitempty"`
	Assignee     string   `json:"assignee,omitempty"`
	Reporter     string   `json:"reporter,omitempty"`
	Labels       []string `json:"labels,omitempty"`
	URL          string   `json:"url,omitempty"`
}

// JiraCommentCreated is the body of jira.comment.created.v1.
type JiraCommentCreated struct {
	Request
	JiraRef
	CommentID string `json:"comment_id,omitempty"`
}

// JiraIssueCreated is the body of jira.issue.created.v1.
type JiraIssueCreated struct {
	Request
	JiraRef
}

// JiraIssueAssigned is the body of jira.issue.assigned.v1: the issue was
// handed to an agent, and nobody typed a message. Agent is that agent, when
// the host knows it.
type JiraIssueAssigned struct {
	Request
	JiraRef
	Agent string `json:"agent,omitempty"`
}

// CronTick is the body of cron.tick.v1: one firing of a schedule.
// Occurrence is the scheduled instant, not the observed one.
type CronTick struct {
	Request
	Schedule   string    `json:"schedule"`
	Expression string    `json:"expression"`
	Occurrence time.Time `json:"occurrence"`
}

func (MessagePosted) Type() string              { return TypeMessagePosted }
func (SlackCommandInvoked) Type() string        { return TypeSlackCommandInvoked }
func (SlackSessionStopped) Type() string        { return TypeSlackSessionStopped }
func (GitHubIssueCommentCreated) Type() string  { return TypeGitHubIssueCommentCreated }
func (GitHubReviewCommentCreated) Type() string { return TypeGitHubReviewCommentCreated }
func (GitHubReviewSubmitted) Type() string      { return TypeGitHubReviewSubmitted }
func (GitHubIssueOpened) Type() string          { return TypeGitHubIssueOpened }
func (GitHubIssueLabeled) Type() string         { return TypeGitHubIssueLabeled }
func (GitHubPullOpened) Type() string           { return TypeGitHubPullOpened }
func (GitHubPullReadyForReview) Type() string   { return TypeGitHubPullReadyForReview }
func (GitHubReviewRequested) Type() string      { return TypeGitHubReviewRequested }
func (GitHubPullSynchronize) Type() string      { return TypeGitHubPullSynchronize }
func (GitHubPullLabeled) Type() string          { return TypeGitHubPullLabeled }
func (JiraCommentCreated) Type() string         { return TypeJiraCommentCreated }
func (JiraIssueCreated) Type() string           { return TypeJiraIssueCreated }
func (JiraIssueAssigned) Type() string          { return TypeJiraIssueAssigned }
func (CronTick) Type() string                   { return TypeCronTick }

func (MessagePosted) isBody()              {}
func (SlackCommandInvoked) isBody()        {}
func (SlackSessionStopped) isBody()        {}
func (GitHubIssueCommentCreated) isBody()  {}
func (GitHubReviewCommentCreated) isBody() {}
func (GitHubReviewSubmitted) isBody()      {}
func (GitHubIssueOpened) isBody()          {}
func (GitHubIssueLabeled) isBody()         {}
func (GitHubPullOpened) isBody()           {}
func (GitHubPullReadyForReview) isBody()   {}
func (GitHubReviewRequested) isBody()      {}
func (GitHubPullSynchronize) isBody()      {}
func (GitHubPullLabeled) isBody()          {}
func (JiraCommentCreated) isBody()         {}
func (JiraIssueCreated) isBody()           {}
func (JiraIssueAssigned) isBody()          {}
func (CronTick) isBody()                   {}

// RequestText is the request message for text and its trigger context: the
// context in sorted key order, then the text. Keys starting with "_" are
// left out.
func RequestText(text string, meta map[string]string) string {
	return renderContext(text, meta)
}

// renderContext is the one place trigger context is rendered for a model.
// The order is sorted so every replay sends the same bytes.
func renderContext(text string, meta map[string]string) string {
	keys := make([]string, 0, len(meta))
	for k := range meta {
		if !strings.HasPrefix(k, "_") {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return text
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("Context from the trigger:")
	for _, k := range keys {
		fmt.Fprintf(&b, "\n- %s: %s", k, meta[k])
	}
	return b.String() + "\n\n" + text
}

// contextMap starts a meta map from the request's context. Typed fields set
// after it win.
func (r Request) contextMap() map[string]string {
	m := make(map[string]string, len(r.Context)+16)
	for k, v := range r.Context {
		m[k] = v
	}
	return m
}

func setNonEmpty(m map[string]string, k, v string) {
	if v != "" {
		m[k] = v
	}
}

// meta adds the trigger context a Slack message carries to m.
func (s SlackRef) meta(m map[string]string) {
	m["slack_team"] = s.Team
	m["slack_channel"] = s.Channel
	m["slack_user"] = s.User
	m["slack_event"] = s.Event
	m["slack_ts"] = s.TS
	setNonEmpty(m, "slack_thread_ts", s.ThreadTS)
	setNonEmpty(m, "slack_channel_type", s.ChannelType)
	setNonEmpty(m, "slack_app_id", s.AppID)
}

func (b MessagePosted) meta() map[string]string {
	m := b.contextMap()
	if b.Slack != nil {
		b.Slack.meta(m)
	}
	return m
}

func (b SlackCommandInvoked) meta() map[string]string {
	m := b.contextMap()
	m["slack_team"] = b.Team
	m["slack_channel"] = b.Channel
	m["slack_user"] = b.User
	m["slack_event"] = "slash_command"
	m["slack_command"] = b.Command
	setNonEmpty(m, "slack_app_id", b.AppID)
	return m
}

// gitHubEvents names the GitHub event and action of each GitHub type.
var gitHubEvents = map[string][2]string{
	TypeGitHubIssueCommentCreated:  {"issue_comment", "created"},
	TypeGitHubReviewCommentCreated: {"pull_request_review_comment", "created"},
	TypeGitHubReviewSubmitted:      {"pull_request_review", "submitted"},
	TypeGitHubIssueOpened:          {"issues", "opened"},
	TypeGitHubIssueLabeled:         {"issues", "labeled"},
	TypeGitHubPullOpened:           {"pull_request", "opened"},
	TypeGitHubPullReadyForReview:   {"pull_request", "ready_for_review"},
	TypeGitHubReviewRequested:      {"pull_request", "review_requested"},
	TypeGitHubPullSynchronize:      {"pull_request", "synchronize"},
	TypeGitHubPullLabeled:          {"pull_request", "labeled"},
}

// GitHubType returns the type of a GitHub event and action, and false when
// no type is registered for them.
func GitHubType(event, action string) (string, bool) {
	for t, ea := range gitHubEvents {
		if ea[0] == event && ea[1] == action {
			return t, true
		}
	}
	return "", false
}

func (g GitHubRef) meta(m map[string]string, typ string, commentID int64) {
	ea := gitHubEvents[typ]
	m["github_event"] = ea[0]
	m["github_action"] = ea[1]
	m["github_owner"] = g.Owner
	m["github_repo"] = g.Repo
	m["github_number"] = strconv.Itoa(g.Number)
	m["github_kind"] = "issue"
	if g.Pull {
		m["github_kind"] = "pull"
	}
	m["github_title"] = g.Title
	m["github_sender"] = g.Sender
	setNonEmpty(m, "github_state", g.State)
	setNonEmpty(m, "github_author", g.Author)
	setNonEmpty(m, "github_url", g.URL)
	setNonEmpty(m, "github_base_ref", g.BaseRef)
	setNonEmpty(m, "github_head_ref", g.HeadRef)
	setNonEmpty(m, "github_head_sha", g.HeadSHA)
	setNonEmpty(m, "github_labels", strings.Join(g.Labels, ","))
	if g.Pull && g.Draft {
		m["github_draft"] = "true"
	}
	if commentID != 0 {
		m["github_comment_id"] = strconv.FormatInt(commentID, 10)
	}
	if g.InstallationID != 0 {
		m["github_installation_id"] = strconv.FormatInt(g.InstallationID, 10)
	}
}

// gitHubMeta is the trigger context of a GitHub body of type typ.
func gitHubMeta(r Request, g GitHubRef, typ string, commentID int64) map[string]string {
	m := r.contextMap()
	g.meta(m, typ, commentID)
	return m
}

func (j JiraRef) meta(m map[string]string) {
	m["jira_event"] = j.WebhookEvent
	m["jira_site"] = j.Site
	m["jira_key"] = j.Key
	setNonEmpty(m, "jira_issue_event", j.IssueEvent)
	setNonEmpty(m, "jira_id", j.ID)
	setNonEmpty(m, "jira_summary", j.Summary)
	setNonEmpty(m, "jira_status", j.Status)
	setNonEmpty(m, "jira_type", j.IssueType)
	setNonEmpty(m, "jira_priority", j.Priority)
	setNonEmpty(m, "jira_project", j.Project)
	setNonEmpty(m, "jira_project_name", j.ProjectName)
	setNonEmpty(m, "jira_assignee", j.Assignee)
	setNonEmpty(m, "jira_reporter", j.Reporter)
	setNonEmpty(m, "jira_labels", strings.Join(j.Labels, ","))
	setNonEmpty(m, "jira_url", j.URL)
}

func jiraMeta(r Request, j JiraRef) map[string]string {
	m := r.contextMap()
	j.meta(m)
	return m
}

// CronOccurrenceFormat is how a tick's occurrence is shown to a model.
const CronOccurrenceFormat = time.RFC3339

func (b CronTick) meta() map[string]string {
	m := b.contextMap()
	m["cron_schedule"] = b.Schedule
	m["cron_expression"] = b.Expression
	m["cron_occurrence"] = b.Occurrence.UTC().Format(CronOccurrenceFormat)
	return m
}

// prompter is an ingress body that asks something of an agent: its request
// text and the trigger context rendered next to it.
type prompter interface {
	Body
	prompt() (string, map[string]string)
}

func (b MessagePosted) prompt() (string, map[string]string)       { return b.message(), b.meta() }
func (b SlackCommandInvoked) prompt() (string, map[string]string) { return b.message(), b.meta() }
func (b CronTick) prompt() (string, map[string]string)            { return b.message(), b.meta() }

func (b GitHubIssueCommentCreated) prompt() (string, map[string]string)  { return gitHubPrompt(b) }
func (b GitHubReviewCommentCreated) prompt() (string, map[string]string) { return gitHubPrompt(b) }
func (b GitHubReviewSubmitted) prompt() (string, map[string]string)      { return gitHubPrompt(b) }
func (b GitHubIssueOpened) prompt() (string, map[string]string)          { return gitHubPrompt(b) }
func (b GitHubIssueLabeled) prompt() (string, map[string]string)         { return gitHubPrompt(b) }
func (b GitHubPullOpened) prompt() (string, map[string]string)           { return gitHubPrompt(b) }
func (b GitHubPullReadyForReview) prompt() (string, map[string]string)   { return gitHubPrompt(b) }
func (b GitHubReviewRequested) prompt() (string, map[string]string)      { return gitHubPrompt(b) }
func (b GitHubPullSynchronize) prompt() (string, map[string]string)      { return gitHubPrompt(b) }
func (b GitHubPullLabeled) prompt() (string, map[string]string)          { return gitHubPrompt(b) }

func (b JiraCommentCreated) prompt() (string, map[string]string) {
	m := jiraMeta(b.Request, b.JiraRef)
	setNonEmpty(m, "jira_comment_id", b.CommentID)
	return b.message(), m
}

func (b JiraIssueCreated) prompt() (string, map[string]string) {
	return b.message(), jiraMeta(b.Request, b.JiraRef)
}

func (b JiraIssueAssigned) prompt() (string, map[string]string) {
	m := jiraMeta(b.Request, b.JiraRef)
	m["jira_assigned"] = "true"
	return b.message(), m
}

// Prompt returns what an ingress body asks: its request text, followed by the
// people, places and links it refers to, and the trigger context a model is
// shown next to it. The catalog renders exactly RequestText(text, context).
// ok is false for a body that asks nothing.
func Prompt(b Body) (text string, context map[string]string, ok bool) {
	p, ok := b.(prompter)
	if !ok {
		return "", nil, false
	}
	text, context = p.prompt()
	return text, context, true
}

// Render renders an event with its catalog entry. An unregistered type is an
// error.
func Render(e Envelope) (string, error) {
	entry, ok := Lookup(e.Type)
	if !ok {
		return "", fmt.Errorf("%w: %s is not registered", ErrInvalid, e.Type)
	}
	return entry.Render(e)
}

// WithRequest returns b with its request replaced by r, and false for a body
// that carries no request.
func WithRequest(b Body, r Request) (Body, bool) {
	switch v := b.(type) {
	case MessagePosted:
		v.Request = r
		return v, true
	case SlackCommandInvoked:
		v.Request = r
		return v, true
	case GitHubIssueCommentCreated:
		v.Request = r
		return v, true
	case GitHubReviewCommentCreated:
		v.Request = r
		return v, true
	case GitHubReviewSubmitted:
		v.Request = r
		return v, true
	case GitHubIssueOpened:
		v.Request = r
		return v, true
	case GitHubIssueLabeled:
		v.Request = r
		return v, true
	case GitHubPullOpened:
		v.Request = r
		return v, true
	case GitHubPullReadyForReview:
		v.Request = r
		return v, true
	case GitHubReviewRequested:
		v.Request = r
		return v, true
	case GitHubPullSynchronize:
		v.Request = r
		return v, true
	case GitHubPullLabeled:
		v.Request = r
		return v, true
	case JiraCommentCreated:
		v.Request = r
		return v, true
	case JiraIssueCreated:
		v.Request = r
		return v, true
	case JiraIssueAssigned:
		v.Request = r
		return v, true
	case CronTick:
		v.Request = r
		return v, true
	}
	return b, false
}

// request renders a body that asks something: its context, then its text.
func request[T prompter]() func(Envelope) (string, error) {
	return render(func(_ Envelope, b T) string { return renderContext(b.prompt()) })
}

// gitHubBody is a GitHub body: its request, its subject and its comment id.
type gitHubBody interface {
	Body
	gitHub() (Request, GitHubRef, int64)
}

func (b GitHubIssueCommentCreated) gitHub() (Request, GitHubRef, int64) {
	return b.Request, b.GitHubRef, b.CommentID
}

func (b GitHubReviewCommentCreated) gitHub() (Request, GitHubRef, int64) {
	return b.Request, b.GitHubRef, b.CommentID
}

func (b GitHubReviewSubmitted) gitHub() (Request, GitHubRef, int64) {
	return b.Request, b.GitHubRef, b.CommentID
}

func (b GitHubIssueOpened) gitHub() (Request, GitHubRef, int64) { return b.Request, b.GitHubRef, 0 }

func (b GitHubIssueLabeled) gitHub() (Request, GitHubRef, int64) { return b.Request, b.GitHubRef, 0 }

func (b GitHubPullOpened) gitHub() (Request, GitHubRef, int64) { return b.Request, b.GitHubRef, 0 }

func (b GitHubPullReadyForReview) gitHub() (Request, GitHubRef, int64) {
	return b.Request, b.GitHubRef, 0
}

func (b GitHubReviewRequested) gitHub() (Request, GitHubRef, int64) {
	return b.Request, b.GitHubRef, 0
}

func (b GitHubPullSynchronize) gitHub() (Request, GitHubRef, int64) {
	return b.Request, b.GitHubRef, 0
}

func (b GitHubPullLabeled) gitHub() (Request, GitHubRef, int64) { return b.Request, b.GitHubRef, 0 }

func gitHubPrompt(b gitHubBody) (string, map[string]string) {
	r, g, comment := b.gitHub()
	m := gitHubMeta(r, g, b.Type(), comment)
	if rc, ok := b.(GitHubReviewCommentCreated); ok && rc.ReviewRootID != 0 {
		m["github_review_root_id"] = strconv.FormatInt(rc.ReviewRootID, 10)
	}
	return r.message(), m
}

func gitHubEntry[T interface {
	gitHubBody
	prompter
}](typ string, newBody func() Body) Entry {
	return Entry{Type: typ, Sensitive: true, New: newBody, Render: request[T]()}
}

func init() {
	for _, e := range []Entry{
		// Every body with a Request carries text a person wrote, or that a
		// person configured, so each is sensitive.
		{Type: TypeMessagePosted, Sensitive: true, New: func() Body { return new(MessagePosted) }, Render: request[MessagePosted]()},
		{Type: TypeSlackCommandInvoked, Sensitive: true, New: func() Body { return new(SlackCommandInvoked) }, Render: request[SlackCommandInvoked]()},
		{Type: TypeSlackSessionStopped, New: func() Body { return new(SlackSessionStopped) },
			Render: render(func(_ Envelope, b SlackSessionStopped) string {
				return line("slack session", b.Channel, b.ThreadTS, "stopped by", b.User)
			})},

		gitHubEntry[GitHubIssueCommentCreated](TypeGitHubIssueCommentCreated, func() Body { return new(GitHubIssueCommentCreated) }),
		gitHubEntry[GitHubReviewCommentCreated](TypeGitHubReviewCommentCreated, func() Body { return new(GitHubReviewCommentCreated) }),
		gitHubEntry[GitHubReviewSubmitted](TypeGitHubReviewSubmitted, func() Body { return new(GitHubReviewSubmitted) }),
		gitHubEntry[GitHubIssueOpened](TypeGitHubIssueOpened, func() Body { return new(GitHubIssueOpened) }),
		gitHubEntry[GitHubIssueLabeled](TypeGitHubIssueLabeled, func() Body { return new(GitHubIssueLabeled) }),
		gitHubEntry[GitHubPullOpened](TypeGitHubPullOpened, func() Body { return new(GitHubPullOpened) }),
		gitHubEntry[GitHubPullReadyForReview](TypeGitHubPullReadyForReview, func() Body { return new(GitHubPullReadyForReview) }),
		gitHubEntry[GitHubReviewRequested](TypeGitHubReviewRequested, func() Body { return new(GitHubReviewRequested) }),
		gitHubEntry[GitHubPullSynchronize](TypeGitHubPullSynchronize, func() Body { return new(GitHubPullSynchronize) }),
		gitHubEntry[GitHubPullLabeled](TypeGitHubPullLabeled, func() Body { return new(GitHubPullLabeled) }),

		{Type: TypeJiraCommentCreated, Sensitive: true, New: func() Body { return new(JiraCommentCreated) }, Render: request[JiraCommentCreated]()},
		{Type: TypeJiraIssueCreated, Sensitive: true, New: func() Body { return new(JiraIssueCreated) }, Render: request[JiraIssueCreated]()},
		{Type: TypeJiraIssueAssigned, Sensitive: true, New: func() Body { return new(JiraIssueAssigned) }, Render: request[JiraIssueAssigned]()},

		{Type: TypeCronTick, Sensitive: true, New: func() Body { return new(CronTick) }, Render: request[CronTick]()},
	} {
		Register(e)
	}
}

// Addressees returns the agents an event names directly: the mentions of its
// request, the agent a review was requested from, the agent an issue was
// assigned to. A body that names nobody returns nil.
func Addressees(b Body) []string {
	r, _ := RequestOf(b)
	out := append([]string(nil), r.Mentions...)
	switch v := b.(type) {
	case GitHubReviewRequested:
		if v.Reviewer != "" {
			out = append(out, v.Reviewer)
		}
	case JiraIssueAssigned:
		if v.Agent != "" {
			out = append(out, v.Agent)
		}
	}
	return out
}

// DirectMessage reports whether an event arrived in a one-to-one
// conversation with the agent, such as a Slack DM.
func DirectMessage(b Body) bool {
	m, ok := b.(MessagePosted)
	return ok && m.Slack != nil && m.Slack.ChannelType == "im"
}
