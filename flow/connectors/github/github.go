// Package github is the GitHub peer in the mesh: trigger, sink and actor.
//
// # Trigger
//
// GitHub delivers events over a webhook signed with HMAC-SHA256 over the body,
// so the connector is an http.Handler and a pkg/connector.Trigger at once:
// Ingest holds the publish function and parks, ServeHTTP authenticates,
// translates and publishes. The event id is the X-GitHub-Delivery GUID, which
// GitHub reuses across retries and manual redeliveries: the property that
// makes it part of a run key rather than merely a unique string.
//
// Each delivery publishes one github.<event>.<action>.v1 event, on the
// channel of its issue or pull request. Five are a human naming agents in
// text: a comment on an issue or pull request, a review, a review comment, a
// new issue, a new pull request. The event lists them as its mentions. The
// sixth is GitHub naming the agent structurally, by requesting a review from
// it: the case with no message at all, where the pull request itself is the
// request.
//
// In the same five, a human may address the app instead (Config.Login) and
// name no agent: "@humagen what changed?". That is published with no
// mentions, and the members of the channel decide whether anyone answers.
// Text that names no agent and does not mention the app publishes nothing.
//
// Automation (Config.Automation) publishes labels and new commits. Everything
// else is acknowledged and dropped, and an event whose sender is a Bot is
// dropped except for a pull request's synchronize event: this connector's own
// comment arrives back as an issue_comment, and acting on it would have the
// mesh answer itself in a loop.
//
// # Sink
//
// One comment per run, edited as the run streams. GitHub renders Markdown, which
// is what the model already writes, so the answer is passed through rather than
// escaped; only the fragments this package interpolates into its own markup are
// neutralized.
//
// # Actor
//
// github_comment, github_request_review and github_add_labels. Each returns the
// address of what it changed, so the action.taken event can cite it.
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/attachment/intake"
	"github.com/urmzd/saige/flow/connector"
	"github.com/urmzd/saige/flow/connectors"
	"github.com/urmzd/saige/flow/connectors/internal/files"
	"github.com/urmzd/saige/flow/connectors/internal/httpjson"
	"github.com/urmzd/saige/flow/connectors/internal/inbound"
	"github.com/urmzd/saige/flow/connectors/internal/render"
	"github.com/urmzd/saige/flow/connectors/internal/sink"
	"github.com/urmzd/saige/flow/connectors/mention"
	"github.com/urmzd/saige/flow/deliverystate"
	"github.com/urmzd/saige/flow/event"
)

// Name is the address scheme this connector owns.
const Name = "github"

// MaxBodyChars is GitHub's limit on a comment body.
const MaxBodyChars = 65536

// Verifier authenticates a raw request body against its headers.
//
// *signature.GitHub satisfies it and is what a deployment should pass. The
// interface is declared here so the connector depends on the check rather than on
// one implementation of it.
type Verifier interface {
	Verify(header http.Header, body []byte) error
}

// Config is everything the GitHub connector needs.
type Config struct {
	// FixPullRequests exposes separately approved fix branches and draft PRs.
	FixPullRequests  bool
	AuthorizeFixPull func(context.Context, Ref) error
	FixPullContext   func(context.Context, Ref) (FixPullContext, error)
	ObserveDelivery  connectors.DeliveryObserver

	// Feedback persists authenticated feedback and stops through the host.
	Feedback connectors.FeedbackHandler
	// StatusReactions replaces textual lifecycle announcements with reactions.
	StatusReactions bool
	// ContinueThread allows unmentioned replies only in an existing agent session.
	ContinueThread func(address.Address) bool

	// Review converts a completed answer into a validated native review. Nil disables it.
	Review func(View, []ContextFile) (Review, bool, error)
	// RecoverReview reconstructs an already-submitted payload after its remote
	// digest is verified. event is its historical approval disposition. This
	// callback grants no new write and must preserve the host's evidence checks.
	// Nil reuses Review, which must then reproduce the same historical payload.
	RecoverReview func(View, []ContextFile, string) (Review, bool, error)

	// ActionRepositories grants native writes from other surfaces to exact owner/repo names.
	ActionRepositories []string

	// DeliveryState persists the run's message handle and rendering across
	// replica changes. Nil keeps local state only. Migrate before construction.
	DeliveryState deliverystate.Store
	// DeliveryScope is a stable identity for this connector instance, such as
	// a client ID. Empty uses the connector name. Do not reuse it across clients.
	DeliveryScope string

	// API talks to GitHub. Required.
	API API

	// Verifier authenticates inbound deliveries. Required for the trigger.
	Verifier Verifier

	// Owners restricts which accounts this connector serves. Empty accepts any,
	// which is right for a single-tenant deployment and wrong for a shared one:
	// with owners listed, a route naming another organization's repository is
	// refused with ErrUndeliverable instead of being attempted with our token.
	Owners []string

	// ReadRepositories grants cross-source read access to exact owner/repo
	// names. It never grants mutation or delivery access.
	ReadRepositories []string

	// Bots maps a GitHub login to the agent it stands for, and is how a review
	// request is resolved: GitHub names the reviewer, not the agent. A GitHub App
	// appears as "docs-bot[bot]" in some payloads and as its slug in a mention,
	// so both forms map here.
	Bots map[string]string

	// Agents is the set of agent names a mention may name. A connector with
	// neither Bots nor Agents names no agent at all: on GitHub "@urmzd" and
	// "@docs-bot" are the same syntax, so an open name set would start a run
	// every time one human tagged another.
	Agents mention.Set

	// Login is the app's own login, which it comments as and is mentioned by:
	// "humagen[bot]", or "humagen". A comment that mentions it is addressed to the
	// app, the way Slack delivers only messages that mention the app, and the
	// mention is taken out of the text the agent reads. Empty means nothing is
	// addressed to the app alone. When Bots maps the app's login, its mention
	// names that agent instead, as any login in Bots does.
	Login string

	// Links is what the deployment knows about the links people paste: which
	// team a Slack domain is, so a pasted Slack permalink is addressed as its
	// thread, and how to read its own URLs, such as a link to a session.
	// GitHub pull request and issue URLs need nothing here.
	Links mention.Links

	// Automation decides whether a labeled or synchronize event asks for
	// work, and with what text. Nil is off. It runs only after signature and
	// repository validation. It names no agent: the members of the pull
	// request's channel decide who answers.
	Automation func(Hook) (text string, ok bool)
	// AutomationFor names the agents an automated event is for, for a host
	// that chooses them itself, such as by label. Nil, or an empty answer,
	// names none.
	AutomationFor func(Hook) []string
	// Prepare applies application policy to a verified event before it is
	// published. An error refuses the delivery.
	Prepare func(context.Context, *event.Envelope) error
	// Render formats the accumulated run view. Nil uses ordinary Markdown.
	Render func(View) string
	// Checks publishes lifecycle progress on the pinned PR head.
	Checks bool
	// Conclusion derives a completed check's outcome. Nil means neutral.
	Conclusion func(View) string

	// Interval coalesces comment edits. Zero means sink.DefaultInterval. GitHub's
	// secondary rate limits are stricter than Slack's about writes to one
	// resource, so a longer interval is reasonable here.
	Interval time.Duration

	// Clock is the time source for coalescing, and the time of an event
	// whose payload carries none. Nil means time.Now.
	Clock func() time.Time

	// MaxBody bounds an inbound request body. Zero means inbound.DefaultMaxBody.
	MaxBody int64

	// Tenant is the authorization scope this connector's events belong to. It
	// is stamped on every event it publishes, files are stored under it, and a
	// thread listing (Messages) for any other tenant is refused.
	Tenant string

	// Files brings in the images and videos referenced in the body that
	// raised an event as the event's attachments (see attachments.go). Needs
	// Tenant. No credential is sent to a file host: public files need none,
	// and a private repository's are fetched through signed URLs read from
	// the API.
	Files connectors.Files
}

// Connector is the GitHub peer.
type Connector struct {
	cfg  Config
	bots map[string]string
	// app is the app's own mention (Config.Login), nil when it has none.
	app           mention.Set
	owners        map[string]bool
	gate          inbound.Gate
	pipe          inbound.Pipe
	engine        *sink.Engine
	checks        *sink.Engine
	reviewsSink   *sink.Engine
	reactionsSink *sink.Engine
	feedbackState deliverystate.Store
	readRepos     map[string]bool
	// fetcher downloads attached files; nil when file intake is off.
	fetcher *intake.Fetcher
	// redirectForTest, set only by tests, points a GitHub file URL at a
	// local server. It is nil in every deployment.
	redirectForTest func(string) string
}

var (
	_ connector.Trigger = (*Connector)(nil)
	_ connector.Sink    = (*Connector)(nil)
	_ connector.Actor   = (*Connector)(nil)
	_ http.Handler      = (*Connector)(nil)
)

// New returns a GitHub connector.
//
//nolint:gocyclo // one check per Config field, in the order the struct declares them
func New(cfg Config) (*Connector, error) {
	if cfg.DeliveryState != nil && cfg.Tenant == "" {
		return nil, fmt.Errorf("github: delivery state requires tenant")
	}
	if cfg.API == nil {
		return nil, fmt.Errorf("github: config needs an API")
	}
	if cfg.FixPullRequests {
		if _, ok := cfg.API.(FixPullAPI); !ok || cfg.AuthorizeFixPull == nil {
			return nil, fmt.Errorf("github: fix PRs require FixPullAPI and host authorization")
		}
	}
	c := &Connector{cfg: cfg}
	c.feedbackState = cfg.DeliveryState
	if c.feedbackState == nil {
		c.feedbackState = deliverystate.NewMemory()
	}
	c.readRepos = make(map[string]bool, len(cfg.ReadRepositories))
	for _, repo := range cfg.ReadRepositories {
		parts := strings.Split(repo, "/")
		if len(parts) != 2 || address.ValidWorkspace(parts[0]) != nil || !validRepository(parts[1]) {
			return nil, fmt.Errorf("github: invalid read repository %q", repo)
		}
		c.readRepos[strings.ToLower(repo)] = true
	}

	c.bots = make(map[string]string, len(cfg.Bots))
	for login, agent := range cfg.Bots {
		if !mention.Valid(agent) {
			return nil, fmt.Errorf("github: bots[%q] = %q is not a usable agent name", login, agent)
		}
		c.bots[strings.ToLower(login)] = agent
		// A GitHub App's login carries a "[bot]" suffix in payloads but not in a
		// mention, so both spellings resolve without the operator listing each.
		c.bots[strings.ToLower(strings.TrimSuffix(login, "[bot]"))] = agent
	}
	if cfg.Login != "" {
		// The app is mentioned without the "[bot]" suffix its payloads carry.
		handle := strings.TrimSuffix(cfg.Login, "[bot]")
		if !mention.Valid(handle) || address.ValidWorkspace(handle) != nil {
			return nil, fmt.Errorf("github: login %q is not a GitHub login", cfg.Login)
		}
		c.app = mention.NewNames(handle)
	}
	if len(cfg.Owners) > 0 {
		c.owners = make(map[string]bool, len(cfg.Owners))
		for _, owner := range cfg.Owners {
			if err := address.ValidWorkspace(owner); err != nil {
				return nil, fmt.Errorf("github: owner %q cannot be an address workspace: %w", owner, err)
			}
			c.owners[strings.ToLower(owner)] = true
		}
	}

	if cfg.Files.Enabled() {
		if cfg.Tenant == "" {
			return nil, fmt.Errorf("github: file intake needs a tenant to store files under")
		}
		c.fetcher = intake.NewFetcher(cfg.Files.Store, fileAllowed,
			intake.WithHeaders(noHeaders),
			intake.WithMaxBytes(cfg.Files.Caps.MaxFileBytes))
	}
	c.gate = inbound.Gate{Verifier: cfg.Verifier, MaxBody: cfg.MaxBody}
	interval := cfg.Interval
	if interval == 0 {
		interval = sink.DefaultInterval
	}
	c.engine = sink.New(surface{c: c}, c.body, sink.WithInterval(interval), sink.WithClock(cfg.Clock), sink.WithState(cfg.DeliveryState, "comments:"+deliveryScope(cfg.Tenant, cfg.DeliveryScope)), sink.WithObserver(c.observed))
	if cfg.Checks {
		api, ok := cfg.API.(CheckAPI)
		if !ok {
			return nil, fmt.Errorf("github: checks need CheckAPI")
		}
		c.checks = sink.New(checkSurface{c: c, api: api}, c.checkBody, sink.WithInterval(interval), sink.WithClock(cfg.Clock), sink.WithState(cfg.DeliveryState, "checks:"+deliveryScope(cfg.Tenant, cfg.DeliveryScope)))
	}
	if cfg.StatusReactions {
		api, ok := cfg.API.(ReactionAPI)
		if !ok || cfg.Login == "" {
			return nil, fmt.Errorf("github: status reactions require ReactionAPI and app login")
		}
		c.reactionsSink = sink.New(reactionSurface{api: api, login: cfg.Login}, reactionBody, sink.WithInterval(interval), sink.WithClock(cfg.Clock), sink.WithState(cfg.DeliveryState, "reactions:"+deliveryScope(cfg.Tenant, cfg.DeliveryScope)))
	}
	if cfg.Review != nil {
		api, ok := cfg.API.(ReviewAPI)
		if !ok {
			return nil, fmt.Errorf("github: native reviews require ReviewAPI")
		}
		history, ok := cfg.API.(ReviewHistoryAPI)
		if !ok {
			return nil, fmt.Errorf("github: native reviews require ReviewHistoryAPI")
		}
		contextAPI, ok := cfg.API.(ContextAPI)
		if !ok {
			return nil, fmt.Errorf("github: native reviews require ContextAPI")
		}
		if cfg.Login == "" {
			return nil, fmt.Errorf("github: native reviews require the app login for recovery")
		}
		c.reviewsSink = sink.New(reviewSurface{c: c, api: api, history: history, context: contextAPI}, reviewBody, sink.WithInterval(interval), sink.WithClock(cfg.Clock), sink.WithState(cfg.DeliveryState, "reviews:"+deliveryScope(cfg.Tenant, cfg.DeliveryScope)))
	}
	return c, nil
}

// Name implements connector.Connector.
func (c *Connector) Name() string { return Name }

// Ingest implements connector.Trigger.
func (c *Connector) Ingest(ctx context.Context, publish connector.Publish) error {
	return c.pipe.Serve(ctx, publish)
}

// Ingesting reports whether an Ingest is running, so a server can answer a
// readiness probe honestly.
func (c *Connector) Ingesting() bool { return c.pipe.Ingesting() }

// ServeHTTP handles one webhook delivery.
func (c *Connector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, ok := c.gate.Body(w, r)
	if !ok {
		return
	}

	// The ping GitHub sends when a webhook is created carries no event to
	// translate, and answering it is how the UI shows the hook as healthy.
	name := r.Header.Get(HeaderEvent)
	if name == "ping" {
		w.WriteHeader(http.StatusOK)
		return
	}

	var h hook
	if err := json.Unmarshal(body, &h); err != nil {
		http.Error(w, "malformed payload", http.StatusBadRequest)
		return
	}

	if c.serveFeedback(r.Context(), w, name, r.Header.Get(HeaderDelivery), h) {
		return
	}
	e, ok := c.eventFrom(name, r.Header.Get(HeaderDelivery), h)
	if !ok {
		w.WriteHeader(http.StatusOK)
		return
	}
	if e.Time.IsZero() {
		// A payload with no timestamp of its own was observed now.
		e.Time = c.now()
	}
	channel, err := address.Parse(e.Channel)
	if err != nil {
		http.Error(w, "invalid GitHub reference", http.StatusBadRequest)
		return
	}
	// Issue-comment payloads identify a PR but omit its head. Pin it before
	// the event is published, so replay and check delivery never choose a
	// later head.
	if (c.cfg.Checks || c.cfg.Review != nil) && channel.Path[1] == "pull" && channel.Params["head_sha"] == "" {
		api, supported := c.cfg.API.(ContextAPI)
		if !supported {
			http.Error(w, "github: checks need context reads for PR comments", http.StatusBadGateway)
			return
		}
		ref, err := c.ref(channel)
		if err != nil {
			http.Error(w, "invalid PR reference", http.StatusBadRequest)
			return
		}
		got, err := api.ReadContext(r.Context(), ref, 1)
		if err != nil || !validSHA(got.Subject.Head.SHA) {
			http.Error(w, "could not resolve PR head", http.StatusBadGateway)
			return
		}
		e = pinHead(e, channel, got.Subject.Head.SHA)
		channel, _ = address.Parse(e.Channel)
	}
	if c.cfg.Prepare != nil {
		if err := c.cfg.Prepare(r.Context(), &e); err != nil {
			http.Error(w, "could not prepare GitHub request", http.StatusBadGateway)
			return
		}
	}
	var candidates []intake.Candidate
	if ref, err := c.ref(channel); err == nil && c.fetcher != nil {
		kind, id, markdown := bodyOf(name, h)
		bodyCtx, cancelBody := context.WithTimeout(context.WithoutCancel(r.Context()), httpjson.DefaultTimeout)
		candidates = c.candidates(bodyCtx, ref, h.Repository.Private, kind, id, markdown)
		cancelBody()
	}
	ctx, cancel := files.Attach(r.Context(), c.cfg.Files, Name, e.Tenant, channel, &e, candidates)
	defer cancel()
	if err := c.pipe.Send(ctx, e); err != nil {
		http.Error(w, "not ready", inbound.SendStatus(err))
		return
	}
	w.WriteHeader(http.StatusOK)
}

// pinHead sets a pull request's head on an event: the head_sha parameter of
// its channel and source, and HeadSHA in its body.
func pinHead(e event.Envelope, channel address.Address, sha string) event.Envelope {
	at := channel.WithParam("head_sha", sha).String()
	e.Channel, e.Source = at, at
	switch b := e.Data.(type) {
	case event.GitHubIssueCommentCreated:
		b.HeadSHA = sha
		e.Data = b
	case event.GitHubReviewCommentCreated:
		b.HeadSHA = sha
		e.Data = b
	case event.GitHubReviewSubmitted:
		b.HeadSHA = sha
		e.Data = b
	}
	return e
}

// now is the configured clock's time.
func (c *Connector) now() time.Time {
	if c.cfg.Clock != nil {
		return c.cfg.Clock().UTC()
	}
	return time.Now().UTC()
}

// agents is the set a mention or a reviewer login is resolved against: the
// configured bot logins first, then the agent name set.
func (c *Connector) agents() mention.Set { return agentSet{bots: c.bots, agents: c.cfg.Agents} }

type agentSet struct {
	bots   map[string]string
	agents mention.Set
}

// Lookup resolves a login or a mention token to an agent.
//
// The "[bot]" suffix is trimmed before every lookup because GitHub is
// inconsistent about it: requested_reviewer.login for a GitHub App is
// "docs-bot[bot]", while the same app is mentioned in a comment as "@docs-bot".
// Trimming here means an operator configures the login once, in either spelling.
func (s agentSet) Lookup(token string) (string, bool) {
	for _, candidate := range []string{token, strings.TrimSuffix(token, "[bot]")} {
		if agent, ok := s.bots[strings.ToLower(candidate)]; ok {
			return agent, true
		}
		if s.agents != nil {
			if agent, ok := s.agents.Lookup(candidate); ok {
				return agent, true
			}
		}
	}
	return "", false
}

// Deliver implements connector.Sink.
func (c *Connector) Deliver(ctx context.Context, target address.Address, e event.Envelope) error {
	if c.cfg.DeliveryState != nil && e.Tenant != c.cfg.Tenant {
		return fmt.Errorf("%w: delivery tenant does not match connector", connector.ErrUndeliverable)
	}
	if _, err := c.ref(target); err != nil {
		return err
	}
	if _, err := reviewRoot(target); err != nil {
		return err
	}
	commentErr := c.engine.Deliver(ctx, target, e)
	if c.reactionsSink != nil {
		commentErr = errors.Join(commentErr, c.reactionsSink.Deliver(ctx, target, e))
	}
	if c.reviewsSink != nil {
		commentErr = errors.Join(commentErr, c.reviewsSink.Deliver(ctx, target, e))
	}
	if c.checks != nil && target.Path[1] == "pull" && validSHA(target.Params["head_sha"]) {
		return errors.Join(commentErr, c.checks.Deliver(ctx, target, e))
	}
	return commentErr
}

// Flush writes every run whose comment edits are still being coalesced.
func (c *Connector) Flush(ctx context.Context) error {
	err := c.engine.Flush(ctx)
	if c.reactionsSink != nil {
		err = errors.Join(err, c.reactionsSink.Flush(ctx))
	}
	if c.reviewsSink != nil {
		err = errors.Join(err, c.reviewsSink.Flush(ctx))
	}
	if c.checks != nil {
		return errors.Join(err, c.checks.Flush(ctx))
	}
	return err
}

// ref reads a github address: github://<owner>/<repo>/{issues|pull}/<number>.
func (c *Connector) ref(target address.Address) (Ref, error) {
	if target.Connector != Name {
		return Ref{}, fmt.Errorf("%w: %s is not a github address", connector.ErrUndeliverable, target)
	}
	if c.owners != nil && !c.owners[strings.ToLower(target.Workspace)] {
		return Ref{}, fmt.Errorf("%w: %s is owned by %q, which this connector does not serve",
			connector.ErrUndeliverable, target, target.Workspace)
	}
	if len(target.Path) != 3 {
		return Ref{}, fmt.Errorf("%w: %s is not github://<owner>/<repo>/{issues|pull}/<number>",
			connector.ErrUndeliverable, target)
	}
	if address.ValidWorkspace(target.Workspace) != nil || !validRepository(target.Path[0]) {
		return Ref{}, fmt.Errorf("%w: invalid GitHub repository", connector.ErrUndeliverable)
	}
	switch target.Path[1] {
	case "issues", "pull":
	default:
		return Ref{}, fmt.Errorf("%w: %s names %q, want issues or pull",
			connector.ErrUndeliverable, target, target.Path[1])
	}
	number, err := strconv.Atoi(target.Path[2])
	if err != nil || number <= 0 {
		return Ref{}, fmt.Errorf("%w: %s does not end in an issue or pull request number",
			connector.ErrUndeliverable, target)
	}
	return Ref{Owner: target.Workspace, Repo: target.Path[0], Number: number}, nil
}

// surface adapts the GitHub API to the sink engine. The handle is the comment
// id, rendered as a string because that is what the engine carries.
type surface struct{ c *Connector }

func (s surface) Create(ctx context.Context, target address.Address, body string) (string, error) {
	ref, err := s.c.ref(target)
	if err != nil {
		return "", err
	}
	root, err := reviewRoot(target)
	if err != nil {
		return "", err
	}
	var id int64
	if root != 0 {
		api, ok := s.c.cfg.API.(ReplyAPI)
		if !ok {
			return "", fmt.Errorf("%w: GitHub review replies", connector.ErrUnsupported)
		}
		id, err = api.CreateReviewReply(ctx, ref, root, body)
	} else {
		id, err = s.c.cfg.API.CreateComment(ctx, ref, body)
	}
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(id, 10), nil
}

func (s surface) Update(ctx context.Context, target address.Address, handle, body string) error {
	ref, err := s.c.ref(target)
	if err != nil {
		return err
	}
	id, err := strconv.ParseInt(handle, 10, 64)
	if err != nil {
		return fmt.Errorf("github: comment handle %q is not an id: %w", handle, err)
	}
	root, err := reviewRoot(target)
	if err != nil {
		return err
	}
	if root != 0 {
		api, ok := s.c.cfg.API.(ReplyAPI)
		if !ok {
			return fmt.Errorf("%w: GitHub review replies", connector.ErrUnsupported)
		}
		return api.UpdateReviewComment(ctx, ref, id, body)
	}
	return s.c.cfg.API.UpdateComment(ctx, ref, id, body)
}

// body renders a run as a GitHub comment.
//
// Nothing is escaped: GitHub renders Markdown, the model writes Markdown, and
// escaping the answer would show a reader the asterisks instead of the emphasis.
// The fragments this package interpolates into its own markup are neutralized in
// markdown.go instead.
func (c *Connector) body(v render.View) string {
	if c.cfg.StatusReactions && v.Empty() && !v.Terminal() && v.Status != render.StatusParked {
		return ""
	}
	if c.cfg.Render != nil {
		return c.cfg.Render(v)
	}
	return RenderDefault(v)
}

// Actions implements connector.Actor.
func (c *Connector) Actions() []connector.Action {
	actions := []connector.Action{
		{
			Name:        "github_read_context",
			Description: "Read the issue or pull request, conversation, reviews, diff comments and changed-file patches in this run's repository. Follow next_page until absent. Missing patches are unavailable evidence, not proof that a file has no changes.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"address":{"type":"string","description":"Optional GitHub issue/PR URL or github:// address. Other repositories require an operator read grant."},"number":{"type":"integer","minimum":1,"description":"Issue or PR number. Defaults to this run's subject."},"page":{"type":"integer","minimum":1,"description":"Page to read, starting at 1."}},"additionalProperties":false}`),
			Invoke:      c.readContext,
		},
		{
			Name: "github_comment",
			Description: "Post a comment on a GitHub issue or pull request. Use this " +
				"to leave a durable note for humans; the run's answer is already " +
				"delivered as its own comment and does not need reposting.",
			Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "address": {"type":"string", "description":"Optional GitHub issue or PR address; other repositories require a write grant."},
    "body": {"type": "string", "description": "Comment body, GitHub Markdown."},
    "number": {"type": "integer", "description": "Issue or pull request number. Defaults to the one the run is answering on."}
  },
  "required": ["body"],
  "additionalProperties": false
}`),
			Invoke: c.comment,
		},
		{
			Name: "github_request_review",
			Description: "Request a review on a pull request from users or teams. Use " +
				"it when the change needs a human who has not been asked yet.",
			Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "address": {"type":"string", "description":"Optional GitHub issue or PR address; other repositories require a write grant."},
    "reviewers": {"type": "array", "items": {"type": "string"}, "description": "GitHub logins to request a review from."},
    "teams": {"type": "array", "items": {"type": "string"}, "description": "Team slugs to request a review from."},
    "number": {"type": "integer", "description": "Pull request number. Defaults to the one the run is answering on."}
  },
  "additionalProperties": false
}`),
			Invoke: c.requestReview,
		},
		{
			Name: "github_add_labels",
			Description: "Add labels to a GitHub issue or pull request. Existing labels " +
				"are kept, so this is safe to call more than once.",
			Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "address": {"type":"string", "description":"Optional GitHub issue or PR address; other repositories require a write grant."},
    "labels": {"type": "array", "items": {"type": "string"}, "description": "Label names to add."},
    "number": {"type": "integer", "description": "Issue or pull request number. Defaults to the one the run is answering on."}
  },
  "required": ["labels"],
  "additionalProperties": false
}`),
			Invoke: c.addLabels,
		},
	}
	actions = append(actions, c.fileAction(), c.workflowStatusAction(), c.reviewAction())
	if c.cfg.FixPullRequests {
		actions = append(actions, c.fixBaseAction(), c.fixPullAction())
	}
	return actions
}

func (c *Connector) comment(ctx context.Context, target address.Address, raw json.RawMessage) (connector.Result, error) {
	var args struct {
		Address string `json:"address"`
		Body    string `json:"body"`
		Number  int    `json:"number"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return connector.Result{}, fmt.Errorf("github: github_comment args: %w", err)
	}
	if strings.TrimSpace(args.Body) == "" {
		return connector.Result{}, fmt.Errorf("github: github_comment needs a body")
	}
	target, targetErr := c.actionTarget(target, args.Address)
	if targetErr != nil {
		return connector.Result{}, targetErr
	}
	ref, kind, err := c.resolve(target, args.Number)
	if err != nil {
		return connector.Result{}, err
	}
	id, err := c.cfg.API.CreateComment(ctx, ref, args.Body)
	if err != nil {
		return connector.Result{}, err
	}
	return connector.Result{
		Summary: fmt.Sprintf("commented on %s", ref),
		Address: refAddress(ref, kind).WithParam("comment", strconv.FormatInt(id, 10)),
	}, nil
}

func (c *Connector) requestReview(ctx context.Context, target address.Address, raw json.RawMessage) (connector.Result, error) {
	var args struct {
		Address   string   `json:"address"`
		Reviewers []string `json:"reviewers"`
		Teams     []string `json:"teams"`
		Number    int      `json:"number"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return connector.Result{}, fmt.Errorf("github: github_request_review args: %w", err)
	}
	if len(args.Reviewers) == 0 && len(args.Teams) == 0 {
		return connector.Result{}, fmt.Errorf("github: github_request_review needs a reviewer or a team")
	}
	target, targetErr := c.actionTarget(target, args.Address)
	if targetErr != nil {
		return connector.Result{}, targetErr
	}
	ref, _, err := c.resolve(target, args.Number)
	if err != nil {
		return connector.Result{}, err
	}
	if err := c.cfg.API.RequestReview(ctx, ref, args.Reviewers, args.Teams); err != nil {
		return connector.Result{}, err
	}
	return connector.Result{
		Summary: fmt.Sprintf("requested review on %s from %s", ref, strings.Join(append(args.Reviewers, args.Teams...), ", ")),
		// A review request is always about a pull request, whatever the run's
		// surface was called.
		Address: refAddress(ref, "pull"),
	}, nil
}

func (c *Connector) addLabels(ctx context.Context, target address.Address, raw json.RawMessage) (connector.Result, error) {
	var args struct {
		Address string   `json:"address"`
		Labels  []string `json:"labels"`
		Number  int      `json:"number"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return connector.Result{}, fmt.Errorf("github: github_add_labels args: %w", err)
	}
	if len(args.Labels) == 0 {
		return connector.Result{}, fmt.Errorf("github: github_add_labels needs at least one label")
	}
	target, targetErr := c.actionTarget(target, args.Address)
	if targetErr != nil {
		return connector.Result{}, targetErr
	}
	ref, kind, err := c.resolve(target, args.Number)
	if err != nil {
		return connector.Result{}, err
	}
	if err := c.cfg.API.AddLabels(ctx, ref, args.Labels); err != nil {
		return connector.Result{}, err
	}
	return connector.Result{
		Summary: fmt.Sprintf("labelled %s %s", ref, strings.Join(args.Labels, ", ")),
		Address: refAddress(ref, kind),
	}, nil
}

// resolve settles which resource an action acts on.
//
// An action may name another issue or pull request in the same repository, and
// may not name another repository or owner: the route decided which repository
// this run can touch, and an argument the model produced must not be able to move
// that boundary.
func (c *Connector) resolve(target address.Address, number int) (Ref, string, error) {
	ref, err := c.ref(target)
	if err != nil {
		return Ref{}, "", err
	}
	kind := target.Path[1]
	if number > 0 && number != ref.Number {
		ref.Number = number
		// A different number may be an issue or a pull request; "issues"
		// addresses both on GitHub, and the sink only ever comments.
		kind = "issues"
	}
	return ref, kind, nil
}

func refAddress(ref Ref, kind string) address.Address {
	if kind != "pull" {
		kind = "issues"
	}
	return address.Address{
		Connector: Name,
		Workspace: ref.Owner,
		Path:      []string{ref.Repo, kind, strconv.Itoa(ref.Number)},
	}
}

func deliveryScope(tenant, scope string) string {
	if scope == "" {
		scope = Name
	}
	key, _ := json.Marshal([]string{Name, tenant, scope})
	return string(key)
}
