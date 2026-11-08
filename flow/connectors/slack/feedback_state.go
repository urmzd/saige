package slack

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/deliverystate"
	"github.com/urmzd/saige/flow/event"
)

type reactionState struct {
	At     time.Time      `json:"at"`
	Source string         `json:"source,omitempty"`
	Value  event.Feedback `json:"value"`
}

// Slack's signed event_ts orders this person's reaction transitions. Retain a
// removal watermark even when its addition has not arrived, so late additions
// cannot resurrect it. Equal timestamps are ambiguous and cannot replace state.
func (c *Connector) recordReaction(ctx context.Context, target address.Address, id, stamp string, value event.Feedback) error {
	_, fraction, dot := strings.Cut(stamp, ".")
	if len(stamp) > 32 || !dot || len(fraction) == 0 || len(fraction) > 6 || strings.ContainsAny(stamp, "+-") {
		return nil
	}
	at, ok := parseTS(stamp)
	if !ok || id == "" {
		return nil
	}
	key := deliverystate.Key{Tenant: c.cfg.Tenant, Scope: "feedback:" + deliveryScope(c.cfg.Tenant, c.cfg.DeliveryScope), Run: value.RunID, Channel: target.String(), Target: value.Actor.ID + ":" + value.Rating}
	return c.feedbackState.Update(ctx, key, func(raw []byte) ([]byte, bool, error) {
		var state reactionState
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &state); err != nil {
				return nil, false, err
			}
			if !at.After(state.At) {
				return raw, false, nil
			}
		}
		handler := c.cfg.Feedback
		if value.Removed {
			if state.Source != "" && handler.Withdraw != nil {
				withdrawn := state.Value
				withdrawn.Removed = true
				if err := handler.Withdraw(ctx, c.cfg.Tenant, id, state.Source, withdrawn); err != nil {
					return nil, false, err
				}
			}
			state.Source = ""
		} else if handler.Record != nil {
			if err := handler.Record(ctx, c.cfg.Tenant, id, value); err != nil {
				return nil, false, err
			}
			state.Source, state.Value = id, value
		}
		state.At = at
		data, err := json.Marshal(state)
		return data, false, err
	})
}
