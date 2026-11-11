package slack

import (
	"context"
	"net/http"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connectors/internal/inbound"
	"github.com/urmzd/saige/flow/event"
)

// StopEvent is the Events API type Slack sends when a person presses stop on an
// agent session. Subscribing to it is what makes Slack show the button: without
// the subscription a processing session has a loading indicator and no way out.
const StopEvent = "agent_session_stopped"

// stopFrom reads a stop request out of a verified event as
// slack.session.stopped.v1, published on the session's channel: the channel,
// with ?thread= for a thread session, the same channel a mention in that
// thread is published on. A code channel is its own session and carries no
// thread. ok is false for an event that is not one, or that names no session
// this connector serves.
func (c *Connector) stopFrom(env eventEnvelope) (event.Envelope, bool) {
	ev := env.Event
	if env.Type != typeEventCallback || ev.Type != StopEvent {
		return event.Envelope{}, false
	}
	if env.TeamID == "" || ev.Channel == "" || ev.User == "" {
		return event.Envelope{}, false
	}
	if c.cfg.Team != "" && env.TeamID != c.cfg.Team {
		return event.Envelope{}, false
	}
	if err := address.ValidWorkspace(env.TeamID); err != nil {
		return event.Envelope{}, false
	}
	session := address.Address{Connector: Name, Workspace: env.TeamID, Path: []string{ev.Channel}}
	if ev.ThreadTS != "" {
		session = session.WithParam("thread", ev.ThreadTS)
	}
	id := env.EventID
	if id == "" {
		id = "stop:" + ev.Channel + ":" + firstNonEmpty(ev.EventTS, ev.TS, ev.ThreadTS)
	}
	at := session.String()
	return event.Envelope{
		ID:      id,
		Source:  at,
		Type:    event.TypeSlackSessionStopped,
		Subject: ev.Channel,
		Channel: at,
		Time:    observedAt(env),
		Tenant:  c.cfg.Tenant,
		Data:    event.SlackSessionStopped{Team: env.TeamID, Channel: ev.Channel, ThreadTS: ev.ThreadTS, User: ev.User},
	}, true
}

// serveStop publishes a stop request. The dispatcher stops the runs it
// started on the session's channel.
func (c *Connector) serveStop(ctx context.Context, w http.ResponseWriter, stop event.Envelope) {
	if err := c.pipe.Send(ctx, stop); err != nil {
		http.Error(w, "not ready", inbound.SendStatus(err))
		return
	}
	w.WriteHeader(http.StatusOK)
}
