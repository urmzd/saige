package slack

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/urmzd/saige/flow/connectors/internal/httpjson"
)

// Names from Slack's Web and Events APIs that several files compare against.
const (
	paramChannel        = "channel"
	codeAlreadyReacted  = "already_reacted"
	codeMessageNotFound = "message_not_found"
	typeEventCallback   = "event_callback"
	emojiRaisedHand     = "raised_hand"
)

// API is the slice of Slack's Web API this connector uses: post a message, edit
// it, react to one, report a session's status, register a channel's commands.
// A handful of endpoints, behind an interface, so that every decision above the
// wire — which message a run owns, when to edit it, what the body says — is
// tested without a network and without credentials.
//
// Workspace is on every request so that an implementation serving several Slack
// workspaces can choose a token per request. HTTPAPI holds one token and ignores
// it (see Config.Team).
type API interface {
	// PostMessage posts and returns the message timestamp, which is Slack's
	// handle for editing it later.
	PostMessage(ctx context.Context, m Message) (ts string, err error)
	// UpdateMessage replaces the text of the message at m.TS.
	UpdateMessage(ctx context.Context, m Message) error
	// AddReaction adds an emoji reaction to a message.
	AddReaction(ctx context.Context, r Reaction) error
}

// AgentAPI is the optional agent-session capability, separate from messaging.
type AgentAPI interface {
	// SetStatus reports where an agent session stands, creating the session if
	// the thread or code channel has none yet.
	SetStatus(ctx context.Context, s Session) error
	// SetCommands replaces this agent's slash commands in a code channel. An
	// empty set clears them.
	SetCommands(ctx context.Context, workspace, channel string, commands []Command) error
}

// MaxCommands is Slack's channel-wide limit on registered agent commands.
const MaxCommands = 10

// Message is a message to post or edit.
type Message struct {
	// Workspace is the Slack team id the channel belongs to.
	Workspace string
	Channel   string
	// ThreadTS threads the message under an existing message. Empty posts to
	// the channel.
	ThreadTS string
	// TS identifies the message to edit. Empty posts a new one.
	TS   string
	Text string
}

// Reaction is an emoji reaction on a message.
type Reaction struct {
	Workspace string
	Channel   string
	// TS is the message being reacted to.
	TS string
	// Name is the emoji name without colons ("eyes", not ":eyes:").
	Name string
}

// Status is where an agent session stands, in Slack's vocabulary.
type Status string

// The statuses agents.sessions.setStatus accepts.
const (
	// StatusProcessing shows Slack's "Working…" indicator. Slack reverts it to
	// active after an hour without a refresh.
	StatusProcessing Status = "processing"
	// StatusActive means the turn is over and the agent is ready for the next.
	StatusActive Status = "active"
	// StatusSuspended means the agent is blocked until a human acts.
	StatusSuspended Status = "suspended"
	// StatusClosed means the conversation is finished. It does not archive a
	// code channel.
	StatusClosed Status = "closed"
)

// Session is a status report for one agent session.
type Session struct {
	Workspace string
	Channel   string
	// ThreadTS scopes the session to a thread. Slack requires it in an ordinary
	// channel or a DM and forbids it in a code channel, where the channel
	// itself is the session.
	ThreadTS string
	Status   Status
}

// Command is a slash command an agent offers in a code channel.
type Command struct {
	// Name is the command without its leading slash: "create-pr".
	Name string `json:"name"`
	// Description is shown beside the command in Slack's typeahead. Required.
	Description string `json:"description"`
	// ArgumentHint describes what follows the command: "[title]".
	ArgumentHint string `json:"argument_hint,omitempty"`
}

// SlackBaseURL is the Web API root.
const SlackBaseURL = "https://slack.com/api"

// HTTPAPI is the real API, over net/http.
//
// It holds one bot token and therefore serves one workspace. A deployment
// installed in several workspaces holds one HTTPAPI per workspace and an API
// implementation that dispatches on Message.Workspace.
type HTTPAPI struct {
	client *httpjson.Client
}

// NewHTTPAPI returns an API bound to a bot token (xoxb-...).
//
// The token goes in the Authorization header rather than in the body: Slack
// accepts both, and a token in a body is a token in a request log.
func NewHTTPAPI(token string, httpClient *http.Client) *HTTPAPI {
	return NewHTTPAPIAt(SlackBaseURL, token, httpClient)
}

// NewHTTPAPIAt is NewHTTPAPI against another Web API root: a recorded
// server in a test, or a gateway in front of Slack.
func NewHTTPAPIAt(base, token string, httpClient *http.Client) *HTTPAPI {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	header.Set("Accept", "application/json")
	return &HTTPAPI{client: &httpjson.Client{HTTP: httpClient, Base: strings.TrimRight(base, "/"), Header: header}}
}

// Compile-time proof that the real implementation and the fake answer the same
// interface, which is the only reason the fake is worth anything.
var (
	_ API        = (*HTTPAPI)(nil)
	_ API        = (*Fake)(nil)
	_ FileAPI    = (*HTTPAPI)(nil)
	_ FileAPI    = (*Fake)(nil)
	_ RepliesAPI = (*HTTPAPI)(nil)
	_ RepliesAPI = (*Fake)(nil)
)

// maxReplyPages bounds how many pages of a thread are read. Two hundred
// messages a page makes this a thread of twenty thousand messages, past
// which the listing is refused rather than read forever.
const maxReplyPages = 100

// FileInfo implements FileAPI with files.info. Slack documents it as a GET
// with query parameters; read methods do not all accept a JSON body.
func (a *HTTPAPI) FileInfo(ctx context.Context, _ string, id string) (File, error) {
	var out struct {
		response
		File File `json:"file"`
	}
	q := url.Values{"file": {id}}
	if err := a.get(ctx, "/files.info", q, &out.response, &out); err != nil {
		return File{}, err
	}
	return out.File, nil
}

// Replies implements RepliesAPI with conversations.replies, following the
// cursor until the thread is read.
func (a *HTTPAPI) Replies(ctx context.Context, _ string, channel, ts string) ([]Reply, error) {
	var all []Reply
	cursor := ""
	for range maxReplyPages {
		var out struct {
			response
			Messages []Reply `json:"messages"`
			HasMore  bool    `json:"has_more"`
			Meta     struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		q := url.Values{paramChannel: {channel}, "ts": {ts}, "limit": {"200"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		if err := a.get(ctx, "/conversations.replies", q, &out.response, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Messages...)
		if out.Meta.NextCursor == "" {
			return all, nil
		}
		cursor = out.Meta.NextCursor
	}
	return nil, fmt.Errorf("slack: conversations.replies: thread %s has more than %d pages", ts, maxReplyPages)
}

// get calls a read method with query parameters. status is the embedded
// response envelope of out, checked for ok like every other call.
func (a *HTTPAPI) get(ctx context.Context, path string, q url.Values, status *response, out any) error {
	if err := a.client.Do(ctx, http.MethodGet, path+"?"+q.Encode(), nil, out); err != nil {
		return fmt.Errorf("slack: %s: %w", path, err)
	}
	if !status.OK {
		return &Error{Method: path, Code: status.Error}
	}
	return nil
}

// response is the envelope every Web API method answers with.
//
// Slack reports application failures as HTTP 200 with ok:false, so a caller that
// only checks the status code treats "channel_not_found" and
// "not_in_channel" as success. Every call here checks ok.
type response struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error"`
	Warning string `json:"warning,omitempty"`
	TS      string `json:"ts,omitempty"`
	Channel string `json:"channel,omitempty"`
}

// PostMessage implements API.
func (a *HTTPAPI) PostMessage(ctx context.Context, m Message) (string, error) {
	req := map[string]any{
		paramChannel: m.Channel,
		"text":       m.Text,
		// mrkdwn is the default, but saying so keeps a workspace-level setting
		// from silently turning the rendering into literal asterisks.
		"mrkdwn": true,
	}
	if m.ThreadTS != "" {
		req["thread_ts"] = m.ThreadTS
	}
	var out response
	if err := a.call(ctx, "/chat.postMessage", req, &out); err != nil {
		return "", err
	}
	if out.TS == "" {
		return "", fmt.Errorf("slack: chat.postMessage returned no ts, so the message cannot be edited")
	}
	return out.TS, nil
}

// UpdateMessage implements API.
func (a *HTTPAPI) UpdateMessage(ctx context.Context, m Message) error {
	if m.TS == "" {
		return fmt.Errorf("slack: chat.update needs the ts of the message to edit")
	}
	req := map[string]any{paramChannel: m.Channel, "ts": m.TS, "text": m.Text}
	return a.call(ctx, "/chat.update", req, &response{})
}

// AddReaction implements API.
func (a *HTTPAPI) AddReaction(ctx context.Context, r Reaction) error {
	req := map[string]any{paramChannel: r.Channel, "timestamp": r.TS, "name": r.Name}
	err := a.call(ctx, "/reactions.add", req, &response{})
	// Adding a reaction that is already there is the expected outcome of a
	// retry, not a failure: the action is idempotent by nature and saying so
	// here keeps every caller from special-casing it.
	if err != nil && errorCode(err) == codeAlreadyReacted {
		return nil
	}
	return err
}

// SetStatus implements API.
func (a *HTTPAPI) SetStatus(ctx context.Context, s Session) error {
	req := map[string]any{"channel_id": s.Channel, "status": string(s.Status)}
	if s.ThreadTS != "" {
		req["thread_ts"] = s.ThreadTS
	}
	return a.call(ctx, "/agents.sessions.setStatus", req, &response{})
}

// SetCommands implements API.
func (a *HTTPAPI) SetCommands(ctx context.Context, _, channel string, commands []Command) error {
	if commands == nil {
		// Slack clears the set on an empty array and rejects a missing one.
		commands = []Command{}
	}
	req := map[string]any{"channel_id": channel, "commands": commands}
	return a.call(ctx, "/agents.conversations.setCommands", req, &response{})
}

func (a *HTTPAPI) call(ctx context.Context, path string, in any, out *response) error {
	if err := a.client.Do(ctx, http.MethodPost, path, in, out); err != nil {
		return fmt.Errorf("slack: %s: %w", path, err)
	}
	if !out.OK {
		return &Error{Method: path, Code: out.Error}
	}
	return nil
}

// Error is an application-level Slack failure: HTTP 200 with ok:false.
type Error struct {
	Method string
	Code   string
}

func (e *Error) Error() string {
	return fmt.Sprintf("slack: %s: %s", e.Method, e.Code)
}

// errorCode returns the Slack error code from err, or "".
func errorCode(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}
