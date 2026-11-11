package slack

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connectors"
	"github.com/urmzd/saige/flow/connectors/mention"
	"github.com/urmzd/saige/flow/event"
)

// eventEnvelope is the outer body of an Events API request. Only the fields the
// connector acts on are decoded: an unknown field is not an error, because Slack
// adds them without notice and a strict decoder would turn that into an outage.
type eventEnvelope struct {
	Type string `json:"type"`
	// Challenge answers a url_verification request.
	Challenge string `json:"challenge"`
	// TeamID is the workspace, and becomes the address workspace.
	TeamID string `json:"team_id"`
	// EventID is Slack's own identifier for this delivery and becomes the
	// event's id.
	// Slack reuses it across retries of the same event, which is exactly the
	// property an idempotency key needs.
	EventID string `json:"event_id"`
	// EventTime is when Slack observed the event, in whole seconds.
	EventTime int64 `json:"event_time"`
	// APIAppID identifies the app the event was delivered to.
	APIAppID string `json:"api_app_id"`
	// Authorizations name the installation the event was delivered for,
	// including the app's own bot user, whose mention is nobody's reference.
	Authorizations []struct {
		UserID string `json:"user_id"`
		IsBot  bool   `json:"is_bot"`
	} `json:"authorizations"`

	Event innerEvent `json:"event"`
}

// self is the app's own bot users this delivery was made for.
func (env eventEnvelope) self() []string {
	var out []string
	for _, a := range env.Authorizations {
		if a.IsBot && a.UserID != "" {
			out = append(out, a.UserID)
		}
	}
	return out
}

// innerEvent is the event itself.
type innerEvent struct {
	Reaction string `json:"reaction"`
	Item     struct {
		Type    string `json:"type"`
		Channel string `json:"channel"`
		TS      string `json:"ts"`
	} `json:"item"`
	Type        string `json:"type"`
	Subtype     string `json:"subtype"`
	User        string `json:"user"`
	BotID       string `json:"bot_id"`
	Text        string `json:"text"`
	TS          string `json:"ts"`
	EventTS     string `json:"event_ts"`
	ThreadTS    string `json:"thread_ts"`
	Channel     string `json:"channel"`
	ChannelType string `json:"channel_type"`
	// Files are the files posted with the message, in the order Slack lists
	// them.
	Files []File `json:"files"`
}

// eventFrom translates a Slack event into a message.posted.v1 event, or
// reports that this event publishes nothing.
//
// Everything that decides "nothing" is here rather than spread through the
// handler, because "which events do we ignore" is a security-adjacent question:
// the loop-prevention rule (never act on a bot's message) is the difference
// between a mesh and a runaway.
//
// The event names the agents the message mentions (Config.Bots) and chooses
// none of them: the members of its channel decide who answers.
func (c *Connector) eventFrom(env eventEnvelope) (event.Envelope, bool) {
	if env.Type != typeEventCallback {
		return event.Envelope{}, false
	}
	// One app's signing secret can authenticate multiple installations. A
	// verified delivery still has to belong to this connector's workspace.
	if c.cfg.Team != "" && env.TeamID != c.cfg.Team {
		return event.Envelope{}, false
	}
	ev := env.Event

	switch ev.Type {
	case "app_mention":
		// A mention in a channel: always a candidate.
	case "message":
		// A direct message to the app is a mention by context: there is nobody
		// else in the conversation. In a channel, a plain message is not:
		// answering every message in a channel we are in is not what joining a
		// channel means.
		if ev.ChannelType != "im" && (ev.ThreadTS == "" || c.cfg.ContinueThread == nil || !c.cfg.ContinueThread(env.TeamID, ev.Channel, ev.ThreadTS)) {
			return event.Envelope{}, false
		}
		if ev.Subtype != "" && ev.Subtype != "file_share" {
			// Joins, leaves, edits and the rest are not requests. A file
			// shared with a message is one: its files ride on the event.
			return event.Envelope{}, false
		}
	default:
		return event.Envelope{}, false
	}

	// Loop prevention, and the reason it is unconditional: this connector's sink
	// posts into the same channels its trigger reads. A bot message that reached
	// the agent would let the mesh answer itself.
	if ev.BotID != "" || ev.User == "" {
		return event.Envelope{}, false
	}
	if env.TeamID == "" || ev.Channel == "" {
		return event.Envelope{}, false
	}
	if err := address.ValidWorkspace(env.TeamID); err != nil {
		return event.Envelope{}, false
	}

	text, mentioned, refs := c.reader(env.TeamID, env.self()...).decode(ev.Text)
	mentions := unique(mentioned)
	for _, agent := range mentions {
		text = mention.Strip(text, agent)
	}

	// The thread the answer belongs in: the mention's own thread if it is in one,
	// otherwise the mention itself, so the answer opens a thread under it rather
	// than adding a line to the channel.
	thread := ev.ThreadTS
	if thread == "" {
		thread = ev.TS
	}
	channel := address.Address{
		Connector: Name,
		Workspace: env.TeamID,
		Path:      []string{ev.Channel},
	}
	if thread != "" {
		channel = channel.WithParam("thread", thread)
	}

	id := env.EventID
	if id == "" {
		// Slack always sends event_id for the Events API. If a proxy stripped
		// it, the channel and the event timestamp still identify the event
		// uniquely, which keeps redelivery idempotent.
		id = ev.Channel + ":" + firstNonEmpty(ev.EventTS, ev.TS)
	}

	body := event.MessagePosted{
		Request: event.Request{
			Text: text, Actor: event.Actor{ID: ev.User}, Mentions: mentions,
			References: connectors.References(refs),
		},
		Slack: &event.SlackRef{
			Team:        env.TeamID,
			Channel:     ev.Channel,
			User:        ev.User,
			Event:       ev.Type,
			TS:          ev.TS,
			ThreadTS:    thread,
			ChannelType: ev.ChannelType,
			AppID:       env.APIAppID,
		},
	}
	return event.Envelope{
		ID:      id,
		Source:  channel.String(),
		Type:    body.Type(),
		Subject: ev.Channel,
		Channel: channel.String(),
		Time:    observedAt(env),
		Tenant:  c.cfg.Tenant,
		Data:    body,
	}, true
}

// unique keeps the first of each name, in order.
func unique(names []string) []string {
	var out []string
	for _, n := range names {
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// observedAt is when Slack saw the event. The message timestamp is preferred
// over the envelope's whole-second event_time because it has microsecond
// resolution and is the same value Slack orders the channel by.
func observedAt(env eventEnvelope) time.Time {
	if t, ok := parseTS(firstNonEmpty(env.Event.EventTS, env.Event.TS)); ok {
		return t
	}
	if env.EventTime > 0 {
		return time.Unix(env.EventTime, 0).UTC()
	}
	return time.Time{}
}

// parseTS reads a Slack timestamp, "<seconds>.<microseconds>".
func parseTS(ts string) (time.Time, bool) {
	if ts == "" {
		return time.Time{}, false
	}
	secs, frac, _ := strings.Cut(ts, ".")
	sec, err := strconv.ParseInt(secs, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	var nsec int64
	if frac != "" {
		micro, err := strconv.ParseInt(frac, 10, 64)
		if err != nil {
			return time.Time{}, false
		}
		// Slack's fraction is six digits; pad or truncate rather than assuming.
		for i := len(frac); i < 6; i++ {
			micro *= 10
		}
		nsec = micro * 1000
	}
	return time.Unix(sec, nsec).UTC(), true
}

// reader is what Slack's markup is read against: the configured bots, the
// app's own bot users, the team the text was written in and what the
// deployment knows about links.
type reader struct {
	bots  map[string]string
	self  []string
	team  string
	links mention.Links
}

func (c *Connector) reader(team string, self ...string) reader {
	return reader{bots: c.cfg.Bots, self: self, team: team, links: c.cfg.Links}
}

// decode turns Slack's wire text into text a model can read, and reports which
// agents were mentioned, in the order they appear, and everything the text
// refers to.
//
// Slack sends entity-escaped text with its own markup for anything that refers
// to something: <@U123> for a user, <#C123|general> for a channel,
// <https://x|label> for a link, <!here> for a broadcast. A model reading the raw
// form learns opaque ids and stray angle brackets, so each construct is reduced
// to what a human sees, except a mention of an agent, which is reduced to
// "@name" so that mention stripping can then remove it the same way it does on
// every other surface.
//
// What the reduction drops is kept in refs instead: a user who is not a bot
// is a person with their id, a channel is a place addressed in the team, and
// a link keeps its URL and, through Config.Links, its address. A broadcast
// refers to nobody in particular, and the app's own bot user is the
// addressee rather than someone referred to; neither is a reference.
func (r reader) decode(raw string) (text string, mentioned []string, refs []mention.Reference) {
	var b strings.Builder
	b.Grow(len(raw))

	for i := 0; i < len(raw); {
		if raw[i] != '<' {
			b.WriteByte(raw[i])
			i++
			continue
		}
		end := strings.IndexByte(raw[i:], '>')
		if end < 0 {
			// An unbalanced "<" is literal text, not markup.
			b.WriteString(raw[i:])
			break
		}
		token := raw[i+1 : i+end]
		i += end + 1

		// The text is unescaped once, at the end; a reference is unescaped
		// here, since it never passes through the text.
		body, label, hasLabel := strings.Cut(token, "|")
		switch {
		case strings.HasPrefix(body, "@"):
			id := body[1:]
			if agent, ok := r.bots[id]; ok {
				mentioned = append(mentioned, agent)
				refs = mention.Merge(refs, mention.Reference{Kind: mention.KindAgent, Name: agent, ID: id})
				b.WriteString("@" + agent)
				continue
			}
			if !slices.Contains(r.self, id) {
				person := mention.Reference{Kind: mention.KindPerson, ID: id}
				if hasLabel && label != "" {
					person.Name = "@" + strings.TrimPrefix(unescape(label), "@")
				}
				refs = mention.Merge(refs, person)
			}
			if hasLabel && label != "" {
				b.WriteString("@" + label)
				continue
			}
			// An unresolved user id is still information: someone was named. It
			// is written as "@<id>", which no agent name set will match.
			b.WriteString("@" + id)
		case strings.HasPrefix(body, "#"):
			id := body[1:]
			place := mention.Reference{Kind: mention.KindPlace, ID: id}
			if hasLabel && label != "" {
				place.Name = "#" + unescape(label)
			}
			if r.team != "" && id != "" {
				place.Address = address.Address{Connector: Name, Workspace: r.team, Path: []string{id}}.String()
			}
			refs = mention.Merge(refs, place)
			if hasLabel && label != "" {
				b.WriteString("#" + label)
				continue
			}
			b.WriteString("#" + id)
		case strings.HasPrefix(body, "!"):
			// A broadcast: <!here>, <!channel>, <!subteam^S123|@team>.
			if hasLabel && label != "" {
				b.WriteString(label)
				continue
			}
			b.WriteString("@" + body[1:])
		default:
			// A link. The label is what the human saw; without one, the URL is.
			if link, ok := mention.ParseLink(unescape(body), unescape(label), r.links); ok {
				refs = mention.Merge(refs, link)
			}
			if hasLabel && label != "" {
				b.WriteString(label)
				continue
			}
			b.WriteString(body)
		}
	}

	return unescape(strings.TrimSpace(b.String())), mentioned, refs
}

// unescape reverses the three entities Slack escapes on the way in. It runs
// after markup reduction so that a "&gt;" in the text cannot be mistaken for the
// end of a markup token.
func unescape(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	// Ampersand last: doing it first would turn "&amp;lt;" into "<".
	return strings.ReplaceAll(s, "&amp;", "&")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
