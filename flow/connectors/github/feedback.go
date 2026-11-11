package github

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connectors"
	"github.com/urmzd/saige/flow/connectors/internal/render"
	"github.com/urmzd/saige/flow/event"
)

// serveFeedback consumes a command only after verifying its webhook and
// constraining its repository. A command never reaches the agent as a prompt.
func (c *Connector) serveFeedback(ctx context.Context, w http.ResponseWriter, name, id string, h hook) bool {
	if name == "pull_request_review_thread" {
		return c.serveResolved(ctx, w, id, h)
	}
	if (name != "issue_comment" && name != "pull_request_review_comment") || h.Action != "created" || h.Comment == nil {
		return false
	}
	fields := strings.Fields(h.Comment.Body)
	if len(fields) == 0 || fields[0] != "/feedback" && fields[0] != "/stop" {
		return false
	}
	if h.Sender.Login == "" || h.Sender.Type == "Bot" || strings.HasSuffix(h.Sender.Login, "[bot]") {
		w.WriteHeader(http.StatusOK)
		return true
	}
	handler := c.cfg.Feedback
	target := sourceOf(h.Repository, subjectOf(h))
	if _, err := c.ref(target); err != nil {
		http.Error(w, "repository refused", http.StatusForbidden)
		return true
	}
	if handler.Resolve == nil {
		http.Error(w, "feedback not configured", http.StatusServiceUnavailable)
		return true
	}
	if name == "pull_request_review_comment" && h.Comment.InReplyToID > 0 {
		target = target.WithParam("review_comment", fmt.Sprint(h.Comment.InReplyToID))
	}
	resolved, err := handler.Resolve(ctx, c.cfg.Tenant, target)
	if err != nil {
		http.Error(w, "feedback unavailable", http.StatusServiceUnavailable)
		return true
	}
	if !resolved.Found {
		http.Error(w, "no agent answer in this session", http.StatusNotFound)
		return true
	}
	actor := event.Actor{ID: h.Sender.Login}
	if fields[0] == "/stop" {
		if handler.Stop == nil {
			http.Error(w, "stop not configured", http.StatusServiceUnavailable)
			return true
		}
		err = handler.Stop(ctx, c.cfg.Tenant, resolved.Run, actor)
	} else {
		if len(fields) < 2 || fields[1] != "up" && fields[1] != "down" {
			http.Error(w, "use /feedback up or /feedback down followed by a comment", http.StatusBadRequest)
			return true
		}
		if handler.Record == nil {
			http.Error(w, "feedback not configured", http.StatusServiceUnavailable)
			return true
		}
		if id == "" {
			id = fmt.Sprintf("github:feedback:%s:%d", h.Repository.FullName, h.Comment.ID)
		}
		err = handler.Record(ctx, c.cfg.Tenant, id, event.Feedback{RunID: resolved.Run, SessionID: resolved.Session, MessageID: resolved.Message, FindingID: resolved.Finding, Actor: actor, Rating: fields[1], Comment: strings.Join(fields[2:], " "), Surface: Name})
	}
	if err != nil {
		http.Error(w, "request refused", http.StatusServiceUnavailable)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	return true
}

func (c *Connector) observed(ctx context.Context, target address.Address, handle string, v render.View) error {
	if c.cfg.ObserveDelivery == nil {
		return nil
	}
	ref, err := c.ref(target)
	if err != nil {
		return err
	}
	message := refAddress(ref, target.Path[1]).WithParam("comment", handle)
	if root, _ := target.Param("review_comment"); root != "" {
		message = refAddress(ref, target.Path[1]).WithParam("review_comment", handle)
	}
	return c.cfg.ObserveDelivery(ctx, connectors.Delivery{Tenant: v.Tenant, Run: v.RunID, Channel: v.Channel, Target: target, Message: message})
}

// A signed resolution records the human's explicit disposition of an exact
// delivered finding. A changed head or outdated diff is never acceptance.
func (c *Connector) serveResolved(ctx context.Context, w http.ResponseWriter, id string, h hook) bool {
	if h.Action != "resolved" && h.Action != "unresolved" {
		w.WriteHeader(http.StatusOK)
		return true
	}
	handler := c.cfg.Feedback
	if h.Thread == nil || h.Thread.NodeID == "" || len(h.Thread.Comments) > 100 || h.Sender.Login == "" || h.Sender.Type == "Bot" || strings.HasSuffix(h.Sender.Login, "[bot]") || handler.Resolve == nil || h.Action == "resolved" && handler.Record == nil || h.Action == "unresolved" && handler.Withdraw == nil {
		w.WriteHeader(http.StatusOK)
		return true
	}
	target := sourceOf(h.Repository, subjectOf(h))
	if _, err := c.ref(target); err != nil {
		http.Error(w, "repository refused", http.StatusForbidden)
		return true
	}
	for _, comment := range h.Thread.Comments {
		if comment.ID <= 0 {
			continue
		}
		resolved, err := handler.Resolve(ctx, c.cfg.Tenant, target.WithParam("review_comment", fmt.Sprint(comment.ID)))
		if err != nil {
			http.Error(w, "feedback unavailable", http.StatusServiceUnavailable)
			return true
		}
		if !resolved.Found || resolved.Finding == "" {
			continue
		}
		if id == "" {
			http.Error(w, "resolution needs delivery identity", http.StatusBadRequest)
			return true
		}
		err = c.recordResolution(ctx, target, h.Thread.NodeID, comment.ID, id+":"+fmt.Sprint(comment.ID), event.Feedback{RunID: resolved.Run, SessionID: resolved.Session, MessageID: resolved.Message, FindingID: resolved.Finding, Actor: event.Actor{ID: h.Sender.Login}, Rating: "up", Reasons: []string{"resolved"}, Surface: Name, Removed: h.Action == "unresolved"})
		if err != nil {
			http.Error(w, "feedback refused", http.StatusServiceUnavailable)
			return true
		}
	}
	w.WriteHeader(http.StatusOK)
	return true
}
