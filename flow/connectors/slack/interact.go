package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
)

// InteractivityPath is the path suffix that receives interactivity payloads:
// a button or menu on the app's messages, and its shortcuts. It is what an
// app's interactivity request URL ends in.
const InteractivityPath = "/interactive"

// Interaction kinds.
const (
	// InteractAction is a button or menu on a message.
	InteractAction = "action"
	// InteractMessageShortcut is a shortcut in a message's More actions menu.
	InteractMessageShortcut = "message_shortcut"
	// InteractShortcut is a shortcut from the shortcuts menu.
	InteractShortcut = "shortcut"
)

// Interaction is one thing a person did with the app: pressed a button on
// one of its messages, or chose one of its shortcuts.
type Interaction struct {
	// Kind is InteractAction, InteractMessageShortcut or InteractShortcut.
	Kind string
	// Team, Channel and User are where and who. Channel is empty for a
	// shortcut from the shortcuts menu.
	Team, Channel, User string
	// MessageTS is the message acted on, and ThreadTS its thread.
	MessageTS, ThreadTS string
	// ActionID, BlockID and Value are the pressed element's.
	ActionID, BlockID, Value string
	// CallbackID is the shortcut's.
	CallbackID string
	// Text is the message's text, for a message shortcut.
	Text string
	// TriggerID opens a modal in reply, for a few seconds.
	TriggerID string
}

type interactivePayload struct {
	Type       string `json:"type"`
	CallbackID string `json:"callback_id"`
	TriggerID  string `json:"trigger_id"`
	Team       struct {
		ID string `json:"id"`
	} `json:"team"`
	User struct {
		ID string `json:"id"`
	} `json:"user"`
	Channel struct {
		ID string `json:"id"`
	} `json:"channel"`
	Container struct {
		ChannelID string `json:"channel_id"`
		MessageTS string `json:"message_ts"`
		ThreadTS  string `json:"thread_ts"`
	} `json:"container"`
	Message struct {
		TS       string `json:"ts"`
		ThreadTS string `json:"thread_ts"`
		Text     string `json:"text"`
	} `json:"message"`
	Actions []struct {
		ActionID       string `json:"action_id"`
		BlockID        string `json:"block_id"`
		Value          string `json:"value"`
		SelectedOption struct {
			Value string `json:"value"`
		} `json:"selected_option"`
	} `json:"actions"`
}

// serveInteractive handles one HTTP interactivity request, already
// authenticated: its form's payload field.
func (c *Connector) serveInteractive(w http.ResponseWriter, r *http.Request, body []byte) {
	form, err := url.ParseQuery(string(body))
	if err != nil || form.Get("payload") == "" {
		http.Error(w, "malformed payload", http.StatusBadRequest)
		return
	}
	w.WriteHeader(c.interact(r.Context(), []byte(form.Get("payload"))))
}

// interact reads an interactivity payload, from HTTP or Socket Mode, and
// hands each interaction to the host. It answers the status to acknowledge
// with: 200 for anything handled or not for this app, 400 for a payload it
// cannot read, 500 when the host failed.
func (c *Connector) interact(ctx context.Context, raw []byte) int {
	var p interactivePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return http.StatusBadRequest
	}
	if c.cfg.Interact == nil || (c.cfg.Team != "" && p.Team.ID != c.cfg.Team) || p.User.ID == "" {
		return http.StatusOK
	}
	base := Interaction{
		Team: p.Team.ID, Channel: firstNonEmpty(p.Channel.ID, p.Container.ChannelID), User: p.User.ID,
		MessageTS: firstNonEmpty(p.Message.TS, p.Container.MessageTS), ThreadTS: firstNonEmpty(p.Message.ThreadTS, p.Container.ThreadTS),
		CallbackID: p.CallbackID, TriggerID: p.TriggerID,
	}
	var all []Interaction
	switch p.Type {
	case "block_actions":
		for _, a := range p.Actions {
			one := base
			one.Kind, one.ActionID, one.BlockID, one.Value = InteractAction, a.ActionID, a.BlockID, firstNonEmpty(a.Value, a.SelectedOption.Value)
			all = append(all, one)
		}
	case "message_action":
		one := base
		one.Kind, one.Text = InteractMessageShortcut, p.Message.Text
		all = append(all, one)
	case "shortcut":
		one := base
		one.Kind = InteractShortcut
		all = append(all, one)
	default:
		// View submissions and the rest are not read yet.
		return http.StatusOK
	}
	for _, one := range all {
		if err := c.cfg.Interact(ctx, one); err != nil {
			return http.StatusInternalServerError
		}
	}
	return http.StatusOK
}
