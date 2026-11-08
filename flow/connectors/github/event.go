package github

import (
	"strconv"
	"strings"
	"time"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connectors"
	"github.com/urmzd/saige/flow/connectors/mention"
	"github.com/urmzd/saige/flow/event"
)

// GitHub's webhook headers.
const (
	// HeaderEvent names the event type: issue_comment, pull_request, issues.
	HeaderEvent = "X-GitHub-Event"
	// HeaderDelivery is GitHub's own identifier for this delivery, and becomes
	// the event's id. A redelivery from the UI or a retry after a timeout carries the
	// same GUID, which is what makes the run idempotency key correct rather than
	// merely unique.
	HeaderDelivery = "X-GitHub-Delivery"
	// HeaderHookID identifies the webhook configuration that sent the event.
	HeaderHookID = "X-GitHub-Hook-ID"
)

// hook is the union of the webhook payloads this connector reads. GitHub sends
// one JSON object per event type with a shared shape; decoding the union and
// switching on the event name is simpler than five near-identical structs, and
// an unknown field is ignored rather than rejected.
type hook struct {
	Thread *struct {
		NodeID   string `json:"node_id"`
		Comments []body `json:"comments"`
	} `json:"thread"`
	Action     string     `json:"action"`
	Repository repository `json:"repository"`
	Sender     user       `json:"sender"`

	Issue        *issue   `json:"issue"`
	PullRequest  *pull    `json:"pull_request"`
	Comment      *body    `json:"comment"`
	Review       *body    `json:"review"`
	Label        *label   `json:"label"`
	Reviewer     *user    `json:"requested_reviewer"`
	Team         *teamRef `json:"requested_team"`
	Installation *idOnly  `json:"installation"`
}

type repository struct {
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	Owner    user   `json:"owner"`
	Private  bool   `json:"private"`
}

type user struct {
	Login string `json:"login"`
	// Type is "User", "Bot" or "Organization". A Bot sender is never acted on:
	// this connector's own comments arrive back as issue_comment events, and
	// answering them would make the mesh answer itself.
	Type string `json:"type"`
}

type teamRef struct {
	Slug string `json:"slug"`
}

type idOnly struct {
	ID int64 `json:"id"`
}

type label struct {
	Name string `json:"name"`
}

type issue struct {
	Number int     `json:"number"`
	Title  string  `json:"title"`
	Body   string  `json:"body"`
	State  string  `json:"state"`
	User   user    `json:"user"`
	Labels []label `json:"labels"`
	// PullRequest is present when this "issue" is really a pull request, which
	// is how an issue_comment event on a PR is told apart from one on an issue.
	PullRequest *struct {
		URL string `json:"url"`
	} `json:"pull_request"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type pull struct {
	Number  int     `json:"number"`
	Title   string  `json:"title"`
	Body    string  `json:"body"`
	State   string  `json:"state"`
	Draft   bool    `json:"draft"`
	User    user    `json:"user"`
	Labels  []label `json:"labels"`
	HTMLURL string  `json:"html_url"`
	Base    struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Head struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// body is a comment or a review: an author, some text, an id and a timestamp.
// A review dates itself with submitted_at rather than created_at, so both are
// decoded and whichever is present is used.
type body struct {
	InReplyToID int64     `json:"in_reply_to_id"`
	ID          int64     `json:"id"`
	Body        string    `json:"body"`
	User        user      `json:"user"`
	HTMLURL     string    `json:"html_url"`
	CreatedAt   time.Time `json:"created_at"`
	SubmittedAt time.Time `json:"submitted_at"`
}

// at returns when this comment or review was written.
func (b *body) at() time.Time {
	if b == nil {
		return time.Time{}
	}
	return latest(b.CreatedAt, b.SubmittedAt)
}

// subject is the issue or pull request an event is about, normalized.
type subject struct {
	number int
	title  string
	body   string
	state  string
	author string
	labels []string
	url    string
	// pull reports whether the address should say /pull/ rather than /issues/.
	pull  bool
	base  string
	head  string
	sha   string
	draft bool
	at    time.Time
}

// eventFrom translates a webhook delivery into a github.*.v1 event, or reports
// that it publishes nothing. The event names the agents the request mentions,
// or the agent a review was requested from, and chooses none of them: the
// members of its channel decide who answers.
//
//nolint:gocyclo // one case per GitHub webhook event and action
func (c *Connector) eventFrom(name, delivery string, h hook) (event.Envelope, bool) {
	// Ignore bot conversations to prevent loops. Bot-authored commits may still
	// trigger a review on an explicitly activated PR.
	if h.Sender.Type == "Bot" && (name != "pull_request" || h.Action != "synchronize") {
		return event.Envelope{}, false
	}
	if h.Repository.Name == "" || h.Repository.Owner.Login == "" {
		return event.Envelope{}, false
	}
	if err := address.ValidWorkspace(h.Repository.Owner.Login); err != nil {
		return event.Envelope{}, false
	}
	if c.owners != nil && !c.owners[strings.ToLower(h.Repository.Owner.Login)] {
		return event.Envelope{}, false
	}
	if !validRepository(h.Repository.Name) {
		return event.Envelope{}, false
	}
	typ, known := event.GitHubType(name, h.Action)
	if !known {
		return event.Envelope{}, false
	}

	var (
		subj       subject
		text       string
		mentions   []string
		reviewer   string
		commentID  int64
		reviewRoot int64
		ok         bool
		// raw is the text a person wrote, before any mention is taken out:
		// what references are read from. Automation text is the host's, and
		// refers to nothing on a person's behalf.
		raw string
	)

	switch name {
	case "issue_comment":
		if h.Issue == nil || h.Comment == nil {
			return event.Envelope{}, false
		}
		subj = subjectOf(h)
		commentID = h.Comment.ID
		raw = h.Comment.Body
		if mentions, text, ok = c.asked(h.Comment.Body); !ok {
			if c.cfg.ContinueThread == nil || !c.cfg.ContinueThread(sourceOf(h.Repository, subj)) {
				return event.Envelope{}, false
			}
			text = h.Comment.Body
		}

	case "pull_request_review_comment":
		if h.PullRequest == nil || h.Comment == nil {
			return event.Envelope{}, false
		}
		subj = subjectOf(h)
		commentID = h.Comment.ID
		reviewRoot = h.Comment.ID
		if h.Comment.InReplyToID != 0 {
			reviewRoot = h.Comment.InReplyToID
		}
		if reviewRoot <= 0 || subj.number <= 0 {
			return event.Envelope{}, false
		}
		reviewTarget := sourceOf(h.Repository, subj).WithParam("review_comment", strconv.FormatInt(reviewRoot, 10))
		raw = h.Comment.Body
		if mentions, text, ok = c.asked(h.Comment.Body); !ok {
			if c.cfg.ContinueThread == nil || !c.cfg.ContinueThread(reviewTarget) {
				return event.Envelope{}, false
			}
			text = h.Comment.Body
		}

	case "pull_request_review":
		if h.PullRequest == nil || h.Review == nil {
			return event.Envelope{}, false
		}
		subj = subjectOf(h)
		commentID = h.Review.ID
		raw = h.Review.Body
		if mentions, text, ok = c.asked(h.Review.Body); !ok {
			return event.Envelope{}, false
		}

	case "issues":
		if h.Issue == nil {
			return event.Envelope{}, false
		}
		subj = subjectOf(h)
		switch h.Action {
		case "labeled":
			text, ok = c.automation(h)
			mentions = c.automationFor(h)
		case "opened":
			raw = h.Issue.Title + "\n\n" + h.Issue.Body
			mentions, text, ok = c.asked(raw)
		}
		if !ok {
			return event.Envelope{}, false
		}

	case "pull_request":
		if h.PullRequest == nil {
			return event.Envelope{}, false
		}
		subj = subjectOf(h)
		switch h.Action {
		case "synchronize":
			if subj.draft || subj.state == "closed" || !validSHA(subj.sha) {
				return event.Envelope{}, false
			}
			if text, ok = c.automation(h); !ok {
				return event.Envelope{}, false
			}
			mentions = c.automationFor(h)
		case "labeled":
			if text, ok = c.automation(h); !ok {
				return event.Envelope{}, false
			}
			mentions = c.automationFor(h)
		case "review_requested":
			// The agent is named by GitHub, not by a human: the review was
			// requested from its login. This is the one path where no mention
			// exists, and it is the natural way to ask a reviewer agent.
			login := ""
			if h.Reviewer != nil {
				login = h.Reviewer.Login
			} else if h.Team != nil {
				login = h.Team.Slug
			}
			if reviewer, ok = c.agents().Lookup(login); !ok {
				return event.Envelope{}, false
			}
			// A review request carries no message, so the request IS the pull
			// request: its title and description are what the agent reviews.
			raw = subj.title + "\n\n" + subj.body
			text = mention.Strip(strings.TrimSpace(raw), reviewer)
		case "opened", "ready_for_review":
			raw = subj.title + "\n\n" + subj.body
			if mentions, text, ok = c.asked(raw); !ok {
				return event.Envelope{}, false
			}
		default:
			return event.Envelope{}, false
		}

	default:
		return event.Envelope{}, false
	}

	if subj.number <= 0 {
		return event.Envelope{}, false
	}
	if (h.Action == "labeled" || h.Action == "synchronize") && delivery == "" {
		// A label can be removed and reapplied. A synthesized resource key
		// would collapse distinct requests, so require GitHub's delivery ID.
		return event.Envelope{}, false
	}
	if delivery == "" {
		// GitHub always sends a delivery GUID. Without one, the resource plus
		// the comment or subject identity is the best stable key available.
		delivery = name + ":" + h.Repository.FullName + ":" + strconv.Itoa(subj.number) + ":" + strconv.FormatInt(commentID, 10)
	}
	if name == "pull_request" && h.Action == "synchronize" {
		// Different deliveries for the same commit still represent one automatic
		// review. An explicit human mention retains its independent delivery ID.
		delivery = "github:commit:" + strings.ToLower(h.Repository.Owner.Login+"/"+h.Repository.Name) + ":" + strconv.Itoa(subj.number) + ":" + strings.ToLower(subj.sha)
	}

	// When the event is a comment or a review, its own timestamp is when the
	// human asked; the subject's updated_at is only when the resource last
	// changed, which for a busy pull request is something else entirely.
	at := latest(h.Comment.at(), h.Review.at())
	if at.IsZero() {
		at = subj.at
	}

	channel := sourceOf(h.Repository, subj)
	var root int64
	if name == "pull_request_review_comment" {
		root = reviewRoot
		channel = channel.WithParam("review_comment", strconv.FormatInt(root, 10))
	}

	req := event.Request{
		Text:       strings.TrimSpace(text),
		Actor:      event.Actor{ID: h.Sender.Login, Bot: h.Sender.Type == "Bot"},
		Mentions:   mentions,
		References: connectors.References(c.references(h.Repository, raw)),
	}
	ref := refOf(h, subj)
	var body event.Body
	switch typ {
	case event.TypeGitHubIssueCommentCreated:
		body = event.GitHubIssueCommentCreated{Request: req, GitHubRef: ref, CommentID: commentID}
	case event.TypeGitHubReviewCommentCreated:
		body = event.GitHubReviewCommentCreated{Request: req, GitHubRef: ref, CommentID: commentID, ReviewRootID: root}
	case event.TypeGitHubReviewSubmitted:
		body = event.GitHubReviewSubmitted{Request: req, GitHubRef: ref, CommentID: commentID}
	case event.TypeGitHubIssueOpened:
		body = event.GitHubIssueOpened{Request: req, GitHubRef: ref}
	case event.TypeGitHubIssueLabeled:
		body = event.GitHubIssueLabeled{Request: req, GitHubRef: ref}
	case event.TypeGitHubPullOpened:
		body = event.GitHubPullOpened{Request: req, GitHubRef: ref}
	case event.TypeGitHubPullReadyForReview:
		body = event.GitHubPullReadyForReview{Request: req, GitHubRef: ref}
	case event.TypeGitHubReviewRequested:
		body = event.GitHubReviewRequested{Request: req, GitHubRef: ref, Reviewer: reviewer}
	case event.TypeGitHubPullSynchronize:
		body = event.GitHubPullSynchronize{Request: req, GitHubRef: ref}
	case event.TypeGitHubPullLabeled:
		body = event.GitHubPullLabeled{Request: req, GitHubRef: ref}
	default:
		return event.Envelope{}, false
	}
	return event.Envelope{
		ID:      delivery,
		Source:  channel.String(),
		Type:    typ,
		Subject: strings.Join(channel.Path[1:], "/"),
		Channel: channel.String(),
		Time:    at,
		Tenant:  c.cfg.Tenant,
		Data:    body,
	}, true
}

// refOf is the issue or pull request an event is about, as its body carries
// it.
func refOf(h hook, subj subject) event.GitHubRef {
	ref := event.GitHubRef{
		Owner:   h.Repository.Owner.Login,
		Repo:    h.Repository.Name,
		Number:  subj.number,
		Pull:    subj.pull,
		Title:   subj.title,
		Sender:  h.Sender.Login,
		State:   subj.state,
		Author:  subj.author,
		URL:     subj.url,
		BaseRef: subj.base,
		HeadRef: subj.head,
		HeadSHA: subj.sha,
		Labels:  subj.labels,
		Draft:   subj.pull && subj.draft,
	}
	if h.Installation != nil {
		ref.InstallationID = h.Installation.ID
	}
	return ref
}

// asked is who a human's text addresses: every agent it names, in order,
// with each mention taken out of the text. Text that names no agent and does
// not mention the app (Config.Login) asks no one, so a busy repository
// publishes nothing for every comment. The app's own mention is taken out of
// the text either way, so "@humagen @docs-bot summarize" names docs-bot and
// reads "summarize".
func (c *Connector) asked(text string) (mentions []string, stripped string, ok bool) {
	mentions, stripped = mention.FindAll(text, c.agents())
	toApp := false
	if c.app != nil {
		_, stripped, toApp = mention.Find(stripped, c.app)
	}
	return mentions, stripped, len(mentions) > 0 || toApp
}

// references is what a person's text refers to: the agents and people it
// mentions (a login that names no agent is a person, the app's own is
// neither), the issues and pull requests it names ("#12" in this
// repository, "owner/repo#12" in any), and the links it carries.
func (c *Connector) references(repo repository, text string) []mention.Reference {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return mention.References(text, mention.Scope{
		Agents: c.agents(),
		People: true,
		Self:   c.app,
		Repo:   repo.Owner.Login + "/" + repo.Name,
		Links:  c.cfg.Links,
	})
}

func (c *Connector) automation(h hook) (string, bool) {
	if c.cfg.Automation == nil {
		return "", false
	}
	return c.cfg.Automation(h)
}

// automationFor is who an automated event is for, when the host names them.
func (c *Connector) automationFor(h hook) []string {
	if c.cfg.AutomationFor == nil {
		return nil
	}
	return c.cfg.AutomationFor(h)
}

// subjectOf normalizes the issue or pull request an event is about.
func subjectOf(h hook) subject {
	if h.PullRequest != nil {
		p := h.PullRequest
		return subject{
			number: p.Number,
			title:  p.Title,
			body:   p.Body,
			state:  p.State,
			author: p.User.Login,
			labels: labelNames(p.Labels),
			url:    p.HTMLURL,
			pull:   true,
			base:   p.Base.Ref,
			head:   p.Head.Ref,
			sha:    p.Head.SHA,
			draft:  p.Draft,
			at:     latest(p.UpdatedAt, p.CreatedAt),
		}
	}
	if h.Issue != nil {
		i := h.Issue
		s := subject{
			number: i.Number,
			title:  i.Title,
			body:   i.Body,
			state:  i.State,
			author: i.User.Login,
			labels: labelNames(i.Labels),
			url:    i.HTMLURL,
			// An issue_comment on a pull request arrives as an issue with a
			// pull_request member. Addressing it as an issue would still resolve
			// on GitHub, but it would tell the agent's tools the wrong thing
			// about what it is looking at.
			pull: i.PullRequest != nil,
			at:   latest(i.UpdatedAt, i.CreatedAt),
		}
		return s
	}
	return subject{}
}

// sourceOf builds the address of the issue or pull request: the channel an
// event about it is published on.
func sourceOf(repo repository, subj subject) address.Address {
	kind := "issues"
	if subj.pull {
		kind = "pull"
	}
	source := address.Address{
		Connector: Name, Workspace: repo.Owner.Login,
		Path: []string{repo.Name, kind, strconv.Itoa(subj.number)},
	}
	if subj.sha != "" {
		source = source.WithParam("head_sha", subj.sha)
	}
	return source
}

func labelNames(labels []label) []string {
	if len(labels) == 0 {
		return nil
	}
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if l.Name != "" {
			out = append(out, l.Name)
		}
	}
	return out
}

func latest(times ...time.Time) time.Time {
	var out time.Time
	for _, t := range times {
		if t.After(out) {
			out = t
		}
	}
	return out
}
