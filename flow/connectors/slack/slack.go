// Package slack is the Slack peer in the mesh: trigger, sink and actor.
//
// # Trigger
//
// Slack delivers events over a signed HTTP POST, so the connector is both an
// http.Handler and a pkg/connector.Trigger: Ingest holds the publish function
// and parks, and each request turns into at most one event. Nothing is parsed before the
// signature is checked (see internal/inbound), and nothing downstream re-checks,
// because nothing downstream can.
//
// Two event types publish message.posted.v1: app_mention, which is a mention in
// a channel, and a direct message, which is a mention by context rather than by
// text. Everything else is acknowledged and dropped. A slash command an agent
// registered in a code channel publishes slack.command.invoked.v1; it arrives on CommandsPath, signed the same way
// (see command.go). Messages from a bot are always dropped: a
// mesh whose sink posts into the same channel its trigger reads would otherwise
// answer itself, forever, at whatever rate the model can produce tokens.
//
// # Who is named
//
// A Slack mention is <@U08BOT>, an opaque id rather than a name, so the
// connector reads the agents a message names from Config.Bots alone and lists
// them as the event's mentions. It never chooses an agent: the members of the
// event's channel decide who answers (pkg/membership).
//
// # Sink
//
// One message per (run, thread), edited as the run streams, coalesced to respect
// chat.update's rate limit. That behaviour and its rationale live in
// internal/sink; this package supplies the Slack surface and mrkdwn rendering.
//
// The answer threads under the mention: the event's channel carries the
// mention's own timestamp as ?thread=, and egress renders a run on its
// channel, so a channel that tags an agent fifty times has fifty threads
// rather than one flat conversation.
//
// # Sessions
//
// AgentAPI exposes session status and command registration. Applications decide
// how their run lifecycle maps to those capabilities. A person pressing stop on
// a session arrives as StopEvent and is published as slack.session.stopped.v1
// (see stop.go).
//
// # Files and threads
//
// With Config.Files set, the files on a triggering message become the event's
// attachments (see files.go): downloaded from FileHost with the bot token,
// stored under the connector's tenant, and carried as references. The
// connector also lists a thread for reconciliation (see thread.go), so a run
// is shown what people said in the thread without tagging the agent.
//
// # Actor
//
// slack_post_message and slack_add_reaction. Both are ordinary Slack verbs, and
// both report an address so the resulting action.taken event can cite the message
// it created.
package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/attachment/intake"
	"github.com/urmzd/saige/flow/connector"
	"github.com/urmzd/saige/flow/connectors"
	"github.com/urmzd/saige/flow/connectors/internal/files"
	"github.com/urmzd/saige/flow/connectors/internal/inbound"
	"github.com/urmzd/saige/flow/connectors/internal/render"
	"github.com/urmzd/saige/flow/connectors/internal/sink"
	"github.com/urmzd/saige/flow/connectors/mention"
	"github.com/urmzd/saige/flow/deliverystate"
	"github.com/urmzd/saige/flow/event"
)

// Name is the address scheme this connector owns.
const Name = "slack"

// MaxBodyChars is Slack's limit on a message body. A longer body is rejected
// outright, so the renderer clamps to it rather than losing the message.
const MaxBodyChars = 4000

// Verifier authenticates a raw request body against its headers.
//
// *signature.Slack satisfies it, and is what a deployment should pass. The
// interface is declared here rather than imported so that the connector depends
// on the check rather than on one implementation of it, and so a deployment
// terminating Slack's signature at a gateway can supply its own.
type Verifier interface {
	Verify(header http.Header, body []byte) error
}

// Config is everything the Slack connector needs. The zero value is not usable:
// New validates.
type Config struct {
	ObserveDelivery connectors.DeliveryObserver

	// StatusReactions shows lifecycle on the triggering message and renders concise native replies.
	StatusReactions bool

	// ActionChannels grants native actions from other surfaces to exact channel IDs in Team.
	ActionChannels []string

	// Feedback binds verified native reactions to the host's durable feedback writer.
	Feedback connectors.FeedbackHandler
	// ContinueThread grants plain replies only for a session the host recognizes.
	ContinueThread func(team, channel, thread string) bool

	// DeliveryState persists the run's message handle and rendering across
	// replica changes. Nil keeps local state only. Migrate before construction.
	DeliveryState deliverystate.Store
	// DeliveryScope is a stable identity for this connector instance, such as
	// a client ID. Empty uses the connector name. Do not reuse it across clients.
	DeliveryScope string

	// API talks to Slack. Required. Use NewHTTPAPI in production and NewFake in
	// tests, examples and local runs.
	API API

	// Verifier authenticates inbound requests. Required for the trigger: a
	// connector with no verifier refuses every request rather than trusting it.
	Verifier Verifier

	// Team is the Slack team id this connector serves ("T01ABCDEF"). When set,
	// a delivery to any other workspace is refused with ErrUndeliverable, which
	// is what stops a route in one tenant's agent spec from addressing another
	// tenant's Slack. Empty accepts every workspace, which is only correct when
	// API dispatches on Message.Workspace itself.
	Team string

	// Bots maps a Slack user id to the agent it stands for: "U08BOT" ->
	// "docs-bot". This is the primary way an agent is named, because it is the
	// only one that works with Slack's mention encoding. An app that fronts one
	// agent maps its own bot user id to that agent.
	Bots map[string]string

	// Links is what the deployment knows about the links people paste: which
	// team a Slack domain is, so a message permalink is addressed as its
	// thread (slack://<team>/<channel>?thread=<ts>), and how to read its own
	// URLs, such as a link to a session. The zero value still addresses
	// GitHub pull requests and issues; a permalink keeps only its URL.
	Links mention.Links

	// Prepare applies application policy to a verified event before it is
	// published. An error refuses the request.
	Prepare func(context.Context, *event.Envelope) error

	// Interval coalesces edits. Zero means sink.DefaultInterval.
	Interval time.Duration

	// Clock is the time source for coalescing. Nil means time.Now.
	Clock func() time.Time

	// MaxBody bounds an inbound request body. Zero means inbound.DefaultMaxBody.
	MaxBody int64

	// Tenant is the authorization scope this connector's events belong to. It
	// is stamped on every event it publishes, files are stored under it, and a
	// thread listing (Messages) for any other tenant is refused, because the
	// bot token answers for this tenant's workspace and no other.
	Tenant string

	// Files brings in the files on a triggering message (event.files[]) as
	// the event's attachments. Needs Tenant and FileToken.
	Files connectors.Files

	// FileToken is the bot token (xoxb-...) file downloads are made with. It
	// is sent to FileHost only, over HTTPS, and to nothing a download
	// redirects to. The app needs the files:read scope; without it Slack
	// serves a sign-in page, which is detected and reported as unread.
	FileToken string
	// FileTokenSource, when set, supplies each download's current token.
	// A failed refresh never falls back to FileToken.
	FileTokenSource func(context.Context) (string, error)

	// AppID is this deployment's Slack app id ("A0123..."). A message in a
	// listed thread posted by this app is the deployment's own (see
	// thread.Message.Own). A message from a bot user named in Bots is also
	// its own.
	AppID string
}

// Connector is the Slack peer. It implements pkg/connector's Trigger, Sink and
// Actor faces, and net/http's Handler.
type Connector struct {
	cfg           Config
	gate          inbound.Gate
	pipe          inbound.Pipe
	engine        *sink.Engine
	reactions     *sink.Engine
	feedbackState deliverystate.Store
	// fetcher downloads files; nil when file intake is off.
	fetcher *intake.Fetcher
}

// Compile-time proof of the faces this connector claims. RolesOf derives them
// from the type, so these assertions are the declaration.
var (
	_ connector.Trigger = (*Connector)(nil)
	_ connector.Sink    = (*Connector)(nil)
	_ connector.Actor   = (*Connector)(nil)
	_ http.Handler      = (*Connector)(nil)
)

// New returns a Slack connector.
func New(cfg Config) (*Connector, error) {
	if cfg.DeliveryState != nil && cfg.Tenant == "" {
		return nil, fmt.Errorf("slack: delivery state requires tenant")
	}
	if cfg.API == nil {
		return nil, fmt.Errorf("slack: config needs an API")
	}
	if cfg.Team != "" {
		if err := address.ValidWorkspace(cfg.Team); err != nil {
			return nil, fmt.Errorf("slack: team %q cannot be an address workspace: %w", cfg.Team, err)
		}
	}
	for id, agent := range cfg.Bots {
		if !mention.Valid(agent) {
			return nil, fmt.Errorf("slack: bots[%q] = %q is not a usable agent name", id, agent)
		}
	}

	c := &Connector{cfg: cfg}
	c.feedbackState = cfg.DeliveryState
	if c.feedbackState == nil {
		c.feedbackState = deliverystate.NewMemory()
	}
	if cfg.Files.Enabled() {
		if cfg.Tenant == "" || (cfg.FileToken == "" && cfg.FileTokenSource == nil) {
			return nil, fmt.Errorf("slack: file intake needs a tenant to store files under and a bot token to download them with")
		}
		c.fetcher = intake.NewFetcher(cfg.Files.Store, fileHosts(),
			intake.WithRequestHeaders(fileRequestHeaders(cfg.FileToken, cfg.FileTokenSource)),
			intake.WithMaxBytes(cfg.Files.Caps.MaxFileBytes))
	}
	c.gate = inbound.Gate{Verifier: cfg.Verifier, MaxBody: cfg.MaxBody}
	c.engine = sink.New(surface{c: c}, c.body,
		sink.WithInterval(intervalOr(cfg.Interval)),
		sink.WithClock(cfg.Clock), sink.WithState(cfg.DeliveryState, deliveryScope(cfg.Tenant, cfg.DeliveryScope)), sink.WithObserver(c.observed),
	)
	if cfg.StatusReactions {
		api, err := reactionsAvailable(cfg.API)
		if err != nil {
			return nil, err
		}
		c.reactions = sink.New(reactionSurface{api: api}, reactionBody, sink.WithInterval(intervalOr(cfg.Interval)), sink.WithClock(cfg.Clock), sink.WithState(cfg.DeliveryState, "reactions:"+deliveryScope(cfg.Tenant, cfg.DeliveryScope)))
	}
	return c, nil
}

func intervalOr(d time.Duration) time.Duration {
	if d == 0 {
		return sink.DefaultInterval
	}
	return d
}

// Name implements connector.Connector.
func (c *Connector) Name() string { return Name }

// Ingest implements connector.Trigger. It parks until ctx is cancelled while
// ServeHTTP publishes.
func (c *Connector) Ingest(ctx context.Context, publish connector.Publish) error {
	return c.pipe.Serve(ctx, publish)
}

// Ingesting reports whether an Ingest is running and this connector can
// therefore accept events. A server answers its readiness probe with it: a
// webhook endpoint that is listening but has nowhere to publish is not ready,
// and saying so keeps a load balancer from sending it traffic it will 503.
func (c *Connector) Ingesting() bool { return c.pipe.Ingesting() }

// ServeHTTP handles one Slack request: an Events API delivery, or a slash
// command invocation on CommandsPath.
//
// Slack expects an answer within three seconds and retries otherwise, so the
// handler does exactly three things: authenticate, translate, hand off. The
// handoff blocks on publishing to the bus, bounded by the request context, which
// is back pressure rather than a queue we would then have to make durable.
func (c *Connector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, ok := c.gate.Body(w, r)
	if !ok {
		return
	}
	if strings.HasSuffix(r.URL.Path, CommandsPath) {
		c.serveCommand(w, r, body)
		return
	}

	var env eventEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		http.Error(w, "malformed payload", http.StatusBadRequest)
		return
	}

	// url_verification is how Slack proves the endpoint is ours when it is
	// configured. It is signed like every other request, so it is answered only
	// after the gate accepted it.
	if env.Type == "url_verification" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"challenge": env.Challenge})
		return
	}

	if c.serveReaction(r.Context(), w, env) {
		return
	}
	if stop, ok := c.stopFrom(env); ok {
		stop.Time = orRequested(stop.Time, r.Header)
		c.serveStop(r.Context(), w, stop)
		return
	}

	e, ok := c.eventFrom(env)
	if !ok {
		// Not for us: acknowledge, or Slack retries it three more times.
		w.WriteHeader(http.StatusOK)
		return
	}
	if c.serveThanks(r.Context(), w, env, e) {
		return
	}
	e.Time = orRequested(e.Time, r.Header)
	if c.cfg.Prepare != nil {
		if err := c.cfg.Prepare(r.Context(), &e); err != nil {
			http.Error(w, "request refused", http.StatusForbidden)
			return
		}
	}
	origin, _ := address.Parse(e.Source)
	ctx, cancel := files.Attach(r.Context(), c.cfg.Files, Name, e.Tenant, origin, &e, c.candidates(env.TeamID, env.Event.Files))
	defer cancel()
	if err := c.pipe.Send(ctx, e); err != nil {
		http.Error(w, "not ready", inbound.SendStatus(err))
		return
	}
	w.WriteHeader(http.StatusOK)
}

// Deliver implements connector.Sink.
func (c *Connector) Deliver(ctx context.Context, target address.Address, e event.Envelope) error {
	if c.cfg.DeliveryState != nil && e.Tenant != c.cfg.Tenant {
		return fmt.Errorf("%w: delivery tenant does not match connector", connector.ErrUndeliverable)
	}
	if err := c.reachable(target); err != nil {
		return err
	}
	var reactionErr error
	if c.reactions != nil {
		reactionErr = c.reactions.Deliver(ctx, target, e)
	}
	return errors.Join(reactionErr, c.engine.Deliver(ctx, target, e))
}

// Flush writes every run whose edits are still being coalesced. A server calls
// it on its own ticker and at shutdown; see internal/sink for why the connector
// owns no goroutine of its own.
func (c *Connector) Flush(ctx context.Context) error {
	var reactionErr error
	if c.reactions != nil {
		reactionErr = c.reactions.Flush(ctx)
	}
	return errors.Join(reactionErr, c.engine.Flush(ctx))
}

// reachable reports whether this connector can deliver to target.
func (c *Connector) reachable(target address.Address) error {
	if target.Connector != Name {
		return fmt.Errorf("%w: %s is not a slack address", connector.ErrUndeliverable, target)
	}
	if c.cfg.Team != "" && target.Workspace != c.cfg.Team {
		return fmt.Errorf("%w: %s is in workspace %q, this connector serves %q",
			connector.ErrUndeliverable, target, target.Workspace, c.cfg.Team)
	}
	if len(target.Path) != 1 || target.Path[0] == "" {
		return fmt.Errorf("%w: %s does not name a channel (want slack://<team>/<channel>)",
			connector.ErrUndeliverable, target)
	}
	return nil
}

// surface adapts the Slack API to the sink engine.
type surface struct{ c *Connector }

// Create posts the one message this run owns, in the thread on its channel.
func (s surface) Create(ctx context.Context, target address.Address, body string) (string, error) {
	thread, _ := target.Param("thread")
	return s.c.cfg.API.PostMessage(ctx, Message{
		Workspace: target.Workspace,
		Channel:   target.Path[0],
		ThreadTS:  thread,
		Text:      body,
	})
}

// Update edits it.
func (s surface) Update(ctx context.Context, target address.Address, handle, body string) error {
	return s.c.cfg.API.UpdateMessage(ctx, Message{
		Workspace: target.Workspace,
		Channel:   target.Path[0],
		TS:        handle,
		Text:      body,
	})
}

// body renders a run for Slack.
func (c *Connector) body(v render.View) string {
	if c.cfg.StatusReactions {
		return sink.Clamp(replyBody(v), MaxBodyChars)
	}
	return sink.Clamp(sink.Layout(mrkdwn{}, sink.MapText(v, escape)), MaxBodyChars)
}

// Actions implements connector.Actor.
func (c *Connector) Actions() []connector.Action {
	return []connector.Action{
		{
			Name: "slack_post_message",
			Description: "Post a message to a Slack channel or thread. Use this to " +
				"tell someone something outside the thread the agent is answering in; " +
				"the answer itself is already delivered to the thread and does not " +
				"need to be posted.",
			Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "text": {"type": "string", "description": "Message body, Slack mrkdwn."},
    "channel": {"type": "string", "description": "Channel id. Defaults to the channel the run is answering in."},
    "thread_ts": {"type": "string", "description": "Timestamp of the message to reply under. Omit to post to the channel."}
  },
  "required": ["text"],
  "additionalProperties": false
}`),
			Invoke: c.postMessage,
		},
		{
			Name: "slack_add_reaction",
			Description: "Add an emoji reaction to a Slack message. Use it to " +
				"acknowledge without adding noise to the channel.",
			Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "Emoji name without colons, such as eyes or white_check_mark."},
    "channel": {"type": "string", "description": "Channel id. Defaults to the channel the run is answering in."},
    "ts": {"type": "string", "description": "Timestamp of the message to react to. Defaults to the message that raised the run."}
  },
  "required": ["name"],
  "additionalProperties": false
}`),
			Invoke: c.addReaction,
		},
	}
}

func (c *Connector) postMessage(ctx context.Context, target address.Address, raw json.RawMessage) (connector.Result, error) {
	var args struct {
		Text     string `json:"text"`
		Channel  string `json:"channel"`
		ThreadTS string `json:"thread_ts"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return connector.Result{}, fmt.Errorf("slack: slack_post_message args: %w", err)
	}
	if args.Text == "" {
		return connector.Result{}, fmt.Errorf("slack: slack_post_message needs text")
	}
	channel, workspace, err := c.resolve(target, args.Channel)
	if err != nil {
		return connector.Result{}, err
	}
	if args.ThreadTS == "" {
		args.ThreadTS, _ = target.Param("thread")
	}

	ts, err := c.cfg.API.PostMessage(ctx, Message{
		Workspace: workspace,
		Channel:   channel,
		ThreadTS:  args.ThreadTS,
		Text:      args.Text,
	})
	if err != nil {
		return connector.Result{}, err
	}
	posted := address.Address{Connector: Name, Workspace: workspace, Path: []string{channel}}.WithParam("thread", ts)
	return connector.Result{
		Summary: fmt.Sprintf("posted a message in %s", channel),
		Address: posted,
	}, nil
}

func (c *Connector) addReaction(ctx context.Context, target address.Address, raw json.RawMessage) (connector.Result, error) {
	var args struct {
		Name    string `json:"name"`
		Channel string `json:"channel"`
		TS      string `json:"ts"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return connector.Result{}, fmt.Errorf("slack: slack_add_reaction args: %w", err)
	}
	if args.Name == "" {
		return connector.Result{}, fmt.Errorf("slack: slack_add_reaction needs an emoji name")
	}
	channel, workspace, err := c.resolve(target, args.Channel)
	if err != nil {
		return connector.Result{}, err
	}
	if args.TS == "" {
		args.TS, _ = target.Param("thread")
	}
	if args.TS == "" {
		return connector.Result{}, fmt.Errorf("slack: slack_add_reaction needs the ts of a message")
	}

	if err := c.cfg.API.AddReaction(ctx, Reaction{
		Workspace: workspace,
		Channel:   channel,
		TS:        args.TS,
		Name:      args.Name,
	}); err != nil {
		return connector.Result{}, err
	}
	reacted := address.Address{Connector: Name, Workspace: workspace, Path: []string{channel}}.WithParam("thread", args.TS)
	return connector.Result{
		Summary: fmt.Sprintf("reacted :%s: in %s", args.Name, channel),
		Address: reacted,
	}, nil
}

// resolve settles which channel and workspace an action acts on.
//
// An action defaults to the surface the run is answering on, and may name
// another channel — but never another workspace. Letting an argument choose the
// workspace would let a prompt reach across the tenant boundary the route
// established, which is exactly the boundary an agent's own output must not be
// able to move.
func (c *Connector) resolve(target address.Address, channel string) (string, string, error) {
	if target.Connector != Name {
		if c.cfg.Team == "" || channel == "" {
			return "", "", fmt.Errorf("%w: cross-surface Slack action requires a granted channel", connector.ErrUndeliverable)
		}
		for _, granted := range c.cfg.ActionChannels {
			if granted == channel {
				return channel, c.cfg.Team, nil
			}
		}
		return "", "", fmt.Errorf("%w: Slack channel is not granted for cross-surface actions", connector.ErrUndeliverable)
	}
	if err := c.reachable(target); err != nil {
		return "", "", err
	}
	if channel == "" {
		channel = target.Path[0]
	}
	return channel, target.Workspace, nil
}

func deliveryScope(tenant, scope string) string {
	if scope == "" {
		scope = Name
	}
	key, _ := json.Marshal([]string{Name, tenant, scope})
	return string(key)
}
