package github

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connectors"
	"github.com/urmzd/saige/flow/deliverystate"
	"github.com/urmzd/saige/flow/event"
)

type reactionPoll struct {
	Next     int                      `json:"next"`
	Scan     map[int64]NativeReaction `json:"scan"`
	Previous map[int64]NativeReaction `json:"previous"`
	Active   map[int64]NativeReaction `json:"active"`
}

func reactionFeedback(target connectors.FeedbackTarget, r NativeReaction) (event.Feedback, bool) {
	if r.ID <= 0 || r.User.Type == "Bot" || r.User.Login == "" || strings.HasSuffix(r.User.Login, "[bot]") {
		return event.Feedback{}, false
	}
	rating := map[string]string{"+1": "up", "-1": "down"}[r.Content]
	return event.Feedback{RunID: target.Run, SessionID: target.Session, MessageID: target.Message, FindingID: target.Finding, Actor: event.Actor{ID: r.User.Login}, Rating: rating, Surface: Name}, rating != ""
}

// PollFeedback advances one durable page, up to 100 pages of 100 reactions.
// Removals require two identical complete snapshots, because changing offset
// pages can omit an existing reaction. Failed or partial scans remove nothing.
// The supplied page is a continuation hint; durable progress wins after a crash.
//
//nolint:gocyclo // one pass over a poll: reactions, replies and resolutions, each recorded once
func (c *Connector) PollFeedback(ctx context.Context, target address.Address, page int) (int, error) {
	if page < 1 || page > 100 {
		return 0, fmt.Errorf("github: reaction page must be 1 to 100")
	}
	if _, err := c.ref(target); err != nil {
		return 0, err
	}
	handler := c.cfg.Feedback
	if handler.Resolve == nil || handler.Record == nil {
		return 0, fmt.Errorf("github: feedback handler is not configured")
	}
	resolved, err := handler.Resolve(ctx, c.cfg.Tenant, target)
	if err != nil || !resolved.Found {
		return 0, err
	}
	api, ok := c.cfg.API.(ReactionAPI)
	if !ok {
		return 0, fmt.Errorf("github: API cannot read reactions")
	}
	key := deliverystate.Key{Tenant: c.cfg.Tenant, Scope: "feedback:" + deliveryScope(c.cfg.Tenant, c.cfg.DeliveryScope), Run: resolved.Run, Channel: target.String(), Target: target.String()}
	next := 0
	err = c.feedbackState.Update(ctx, key, func(raw []byte) ([]byte, bool, error) {
		p := reactionPoll{Next: 1}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, false, err
			}
		}
		if p.Next < 1 || p.Next > 100 {
			return nil, false, fmt.Errorf("github: invalid persisted reaction cursor")
		}
		if p.Scan == nil {
			p.Scan = map[int64]NativeReaction{}
		}
		if p.Active == nil {
			p.Active = map[int64]NativeReaction{}
		}
		rows, err := api.ListReactions(ctx, target, p.Next)
		if err != nil {
			return nil, false, err
		}
		if len(rows) > 100 {
			return nil, false, fmt.Errorf("github: reaction page exceeds 100 items")
		}
		for _, r := range rows {
			if _, ok := reactionFeedback(resolved, r); ok {
				p.Scan[r.ID] = r
			}
		}
		if len(rows) == 100 {
			if p.Next == 100 {
				return nil, false, fmt.Errorf("github: feedback exceeds 100 reaction pages")
			}
			p.Next++
			next = p.Next
		} else {
			// Apply additions oldest first, independent of page order. Their native
			// IDs make retries harmless after a callback committed but this state did not.
			ids := slices.Sorted(maps.Keys(p.Scan))
			for _, id := range ids {
				r := p.Scan[id]
				feedback, _ := reactionFeedback(resolved, r)
				if err := handler.Record(ctx, c.cfg.Tenant, fmt.Sprintf("github:reaction:%d", id), feedback); err != nil {
					return nil, false, err
				}
				p.Active[id] = r
				if len(p.Active) > 10000 {
					return nil, false, fmt.Errorf("github: feedback tracking exceeds 10000 reactions")
				}
			}
			if handler.Withdraw != nil && p.Previous != nil && maps.Equal(p.Previous, p.Scan) {
				for _, id := range slices.Sorted(maps.Keys(p.Active)) {
					if _, present := p.Scan[id]; present {
						continue
					}
					feedback, _ := reactionFeedback(resolved, p.Active[id])
					feedback.Removed = true
					source := fmt.Sprintf("github:reaction:%d", id)
					if err := handler.Withdraw(ctx, c.cfg.Tenant, source+":removed", source, feedback); err != nil {
						return nil, false, err
					}
					delete(p.Active, id)
				}
			}
			p.Previous, p.Scan, p.Next = p.Scan, nil, 1
		}
		data, err := json.Marshal(p)
		return data, false, err
	})
	if err == nil && next == 0 {
		err = c.pollSuggestion(ctx, target, resolved)
	}
	return next, err
}
