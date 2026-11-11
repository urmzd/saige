package slack

import (
	"context"
	"fmt"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/attachment"
	"github.com/urmzd/saige/flow/attachment/intake"
	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/thread"
)

// Reply is one message in a thread, as conversations.replies returns it.
type Reply struct {
	TS    string `json:"ts"`
	User  string `json:"user"`
	BotID string `json:"bot_id"`
	AppID string `json:"app_id"`
	Text  string `json:"text"`
	Files []File `json:"files"`
}

// RepliesAPI is the optional capability to read a whole thread: every
// message, the parent first, oldest first. HTTPAPI and Fake implement it. It
// needs the *:history scopes for the conversation types the app is in.
type RepliesAPI interface {
	Replies(ctx context.Context, workspace, channel, ts string) ([]Reply, error)
}

var _ thread.Lister = (*Connector)(nil)

// Messages implements thread.Lister: the Slack thread surface names, as Slack
// holds it now, oldest first.
//
// The message timestamp is the id, which is also what message.posted.v1's slack
// ts carries, so the message that raised a run is recognized. A message posted
// by this app, or by a bot user that stands for an agent, is Own. A file on a
// message is brought in only when the journal first sees that message, and
// under the tenant this connector serves.
func (c *Connector) Messages(ctx context.Context, tenant string, surface address.Address) ([]thread.Message, error) {
	api, ok := c.cfg.API.(RepliesAPI)
	if !ok {
		return nil, fmt.Errorf("slack: this API cannot read a thread")
	}
	if c.cfg.Tenant != "" && tenant != c.cfg.Tenant {
		return nil, fmt.Errorf("slack: tenant %q is not the tenant this connector serves", tenant)
	}
	if err := c.reachable(surface); err != nil {
		return nil, err
	}
	ts, _ := surface.Param(thread.ParamThread)
	if ts == "" {
		return nil, fmt.Errorf("slack: %s names no thread", surface)
	}
	replies, err := api.Replies(ctx, surface.Workspace, surface.Path[0], ts)
	if err != nil {
		return nil, err
	}
	origin := address.Address{Connector: Name, Workspace: surface.Workspace, Path: []string{surface.Path[0]}}.WithParam(thread.ParamThread, ts)
	out := make([]thread.Message, 0, len(replies))
	for _, m := range replies {
		text, _, _ := c.reader(surface.Workspace).decode(m.Text)
		msg := thread.Message{
			ID:    m.TS,
			Text:  text,
			Actor: event.Actor{ID: firstNonEmpty(m.User, m.BotID), Bot: m.BotID != ""},
			Own:   c.own(m),
		}
		if at, ok := parseTS(m.TS); ok {
			msg.At = at
		}
		if cands := c.candidates(surface.Workspace, m.Files); len(cands) > 0 {
			msg.Files = func(ctx context.Context) ([]attachment.Attachment, error) {
				return intake.Collect(ctx, tenant, origin, c.cfg.Files.Caps, cands, c.logSkip(m.TS)), nil
			}
		}
		out = append(out, msg)
	}
	return out, nil
}

// own reports a message this deployment posted.
func (c *Connector) own(m Reply) bool {
	if c.cfg.AppID != "" && m.AppID == c.cfg.AppID {
		return true
	}
	_, agent := c.cfg.Bots[m.User]
	return agent
}

func (c *Connector) logSkip(ts string) func(intake.Skip) {
	log := c.cfg.Files.Log
	if log == nil {
		return nil
	}
	return func(s intake.Skip) {
		log.Warn("connector: a file in the thread was not brought in; the agent is told it could not be read",
			"connector", Name, "message", ts, "file", s.Name, "reason", s.Reason, "error", s.Err)
	}
}
