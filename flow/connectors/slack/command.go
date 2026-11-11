package slack

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connectors"
	"github.com/urmzd/saige/flow/connectors/internal/inbound"
	"github.com/urmzd/saige/flow/connectors/mention"
	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/signature"
)

// CommandsPath is the path suffix that receives slash command invocations. It
// is what an app's code_channels.slash_command_url ends in.
const CommandsPath = "/commands"

// serveCommand handles one slash command invocation, already authenticated.
//
// A command is a mention by another route: the human picked this app's command
// in a code channel. The invocation is handed over as the text a human would
// have typed, "/create-pr fix the login test", so an agent needs no second
// input format to act on it. The members of the channel decide who answers.
func (c *Connector) serveCommand(w http.ResponseWriter, r *http.Request, body []byte) {
	form, err := url.ParseQuery(string(body))
	if err != nil {
		http.Error(w, "malformed payload", http.StatusBadRequest)
		return
	}
	e, ok := c.eventFromCommand(form, r.Header)
	if !ok {
		http.Error(w, "malformed payload", http.StatusBadRequest)
		return
	}
	if c.cfg.Prepare != nil {
		if err := c.cfg.Prepare(r.Context(), &e); err != nil {
			http.Error(w, "command refused", http.StatusForbidden)
			return
		}
	}
	if err := c.pipe.Send(r.Context(), e); err != nil {
		http.Error(w, "not ready", inbound.SendStatus(err))
		return
	}
	// An empty 200 acknowledges without posting anything; the run's own message
	// is the answer.
	w.WriteHeader(http.StatusOK)
}

// eventFromCommand translates an invocation into slack.command.invoked.v1. ok
// is false when the payload is not a usable invocation.
func (c *Connector) eventFromCommand(form url.Values, header http.Header) (event.Envelope, bool) {
	team, channel, user := form.Get("team_id"), form.Get("channel_id"), form.Get("user_id")
	command, trigger := form.Get("command"), form.Get("trigger_id")
	if team == "" || channel == "" || user == "" || trigger == "" || !strings.HasPrefix(command, "/") {
		return event.Envelope{}, false
	}
	if c.cfg.Team != "" && team != c.cfg.Team {
		return event.Envelope{}, false
	}
	if err := address.ValidWorkspace(team); err != nil {
		return event.Envelope{}, false
	}

	args, mentioned, refs := c.reader(team).decode(strings.TrimSpace(form.Get("text")))
	mentions := unique(mentioned)
	for _, agent := range mentions {
		args = mention.Strip(args, agent)
	}
	text := command
	if args != "" {
		text += " " + args
	}

	// No thread: a code channel is the conversation, and its answers are
	// top-level messages.
	at := address.Address{Connector: Name, Workspace: team, Path: []string{channel}}.String()
	body := event.SlackCommandInvoked{
		Request: event.Request{
			Text: text, Actor: event.Actor{ID: user}, Mentions: mentions,
			References: connectors.References(refs),
		},
		Command: command,
		Team:    team,
		Channel: channel,
		User:    user,
		AppID:   form.Get("api_app_id"),
	}
	return event.Envelope{
		// trigger_id is unique per invocation, so a replayed request is the
		// same event.
		ID:      "command:" + trigger,
		Source:  at,
		Type:    body.Type(),
		Subject: channel,
		Channel: at,
		Time:    requestedAt(header),
		Tenant:  c.cfg.Tenant,
		Data:    body,
	}, true
}

// orRequested is t, or when Slack sent the request when the event carried no
// time of its own, as a code channel's stop does not.
func orRequested(t time.Time, header http.Header) time.Time {
	if t.IsZero() {
		return requestedAt(header)
	}
	return t
}

// requestedAt is when Slack sent the request. An invocation carries no
// timestamp of its own, and this header is covered by the signature the gate
// already checked.
func requestedAt(header http.Header) time.Time {
	sec, err := strconv.ParseInt(header.Get(signature.HeaderSlackTimestamp), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}
