package slack

import (
	"context"
	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connectors"
	"github.com/urmzd/saige/flow/connectors/internal/render"
	"github.com/urmzd/saige/flow/event"
	"net/http"
	"strings"
)

// serveReaction accepts feedback only on an answer the host recognizes, after
// signature verification and workspace confinement. Bot reactions never wake work.
func (c *Connector) serveReaction(ctx context.Context, w http.ResponseWriter, env eventEnvelope) bool {
	ev := env.Event
	if ev.Type != "reaction_added" && ev.Type != "reaction_removed" {
		return false
	}
	handler := c.cfg.Feedback
	if handler.Resolve == nil || env.Type != typeEventCallback || env.TeamID == "" || (c.cfg.Team != "" && env.TeamID != c.cfg.Team) || ev.User == "" || ev.BotID != "" || c.cfg.Bots[ev.User] != "" || ev.Item.Type != "message" || ev.Item.Channel == "" || ev.Item.TS == "" {
		w.WriteHeader(http.StatusOK)
		return true
	}
	rating := ""
	switch ev.Reaction {
	case "+1", "thumbsup":
		rating = "up"
	case "-1", "thumbsdown":
		rating = "down"
	case emojiRaisedHand, "hand":
	default:
		w.WriteHeader(http.StatusOK)
		return true
	}
	target := address.Address{Connector: Name, Workspace: env.TeamID, Path: []string{ev.Item.Channel}}.WithParam("thread", ev.Item.TS)
	resolved, err := handler.Resolve(ctx, c.cfg.Tenant, target)
	if err != nil {
		http.Error(w, "feedback unavailable", http.StatusServiceUnavailable)
		return true
	}
	if !resolved.Found {
		w.WriteHeader(http.StatusOK)
		return true
	}
	actor := event.Actor{ID: ev.User}
	if rating == "" {
		if handler.Stop != nil && ev.Type == "reaction_added" {
			err = handler.Stop(ctx, c.cfg.Tenant, resolved.Run, actor)
		}
	} else {
		id := env.EventID
		if id == "" {
			id = "reaction:" + ev.User + ":" + ev.EventTS + ":" + ev.Item.TS + ":" + ev.Reaction + ":" + ev.Type
		}
		err = c.recordReaction(ctx, target, id, ev.EventTS, event.Feedback{RunID: resolved.Run, SessionID: resolved.Session, MessageID: resolved.Message, FindingID: resolved.Finding, Actor: actor, Rating: rating, Surface: Name, Removed: ev.Type == "reaction_removed"})
	}
	if err != nil {
		http.Error(w, "feedback refused", http.StatusServiceUnavailable)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	return true
}

func (c *Connector) observed(ctx context.Context, target address.Address, handle string, v render.View) error {
	if c.cfg.ObserveDelivery == nil {
		return nil
	}
	message := address.Address{Connector: Name, Workspace: target.Workspace, Path: target.Path}.WithParam("thread", handle)
	return c.cfg.ObserveDelivery(ctx, connectors.Delivery{Tenant: v.Tenant, Run: v.RunID, Channel: v.Channel, Target: target, Message: message})
}

// A plain acknowledgement in a known conversation gets one native reaction,
// never another model turn. Messages with files or any substantive text pass.
func (c *Connector) serveThanks(ctx context.Context, w http.ResponseWriter, env eventEnvelope, e event.Envelope) bool {
	if !c.cfg.StatusReactions || c.cfg.ContinueThread == nil || env.Event.ThreadTS == "" || len(env.Event.Files) > 0 || !c.cfg.ContinueThread(env.TeamID, env.Event.Channel, env.Event.ThreadTS) {
		return false
	}
	req, ok := event.RequestOf(e.Data)
	if !ok {
		return false
	}
	text := strings.ToLower(strings.Trim(strings.TrimSpace(req.Text), ".!"))
	switch text {
	case "thanks", "thank you", "thx", "ty":
	default:
		return false
	}
	api, ok := c.cfg.API.(ReactionAPI)
	if !ok {
		return false
	}
	err := api.AddReaction(ctx, Reaction{Workspace: env.TeamID, Channel: env.Event.Channel, TS: env.Event.TS, Name: "heart"})
	if err != nil && errorCode(err) != codeAlreadyReacted {
		http.Error(w, "reaction unavailable", http.StatusServiceUnavailable)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	return true
}
