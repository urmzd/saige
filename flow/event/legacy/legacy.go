// Package legacy reads data written before the event union: bus rows, journal
// entries and tags that carry an envelope.Kind or free text. It reads raw
// JSON only and imports none of the old packages, so it keeps working once
// they are gone.
//
// Every old kind maps to one catalog type, or is dropped. See the table in
// docs/architecture/events.md.
package legacy

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/urmzd/saige/flow/event"
)

// ErrDropped reports an old kind that has no type. Callers match with
// errors.Is.
var ErrDropped = errors.New("legacy: kind was dropped")

// ErrUnknown reports an old kind or selector this package does not know.
var ErrUnknown = errors.New("legacy: unknown kind")

// kinds maps every old envelope.Kind to its type.
var kinds = map[string]string{
	"lifecycle.accepted":     event.TypeAccepted,
	"lifecycle.started":      event.TypeStarted,
	"lifecycle.parked":       event.TypeParked,
	"lifecycle.resumed":      event.TypeResumed,
	"lifecycle.completed":    event.TypeCompleted,
	"lifecycle.failed":       event.TypeFailed,
	"lifecycle.stopped":      event.TypeStopped,
	"lifecycle.child.parked": event.TypeChildParked,

	"delta.text":      event.TypeText,
	"delta.thinking":  event.TypeThinking,
	"delta.tool.call": event.TypeToolCall,
	"delta.tool.done": event.TypeToolDone,
	"delta.citation":  event.TypeCitation,

	"action.taken": event.TypeAction,

	"usage.call":        event.TypeUsageCall,
	"usage.child":       event.TypeUsageChild,
	"context.compacted": event.TypeCompacted,

	"control.stop":    event.TypeStop,
	"control.compact": event.TypeCompact,
}

// dropped are the old kinds with no type: nothing produced them.
var dropped = []string{"control.created", "control.revised", "control.deleted"}

// families maps an old kind family to the type prefixes it selected.
var families = map[string][]string{
	"delta":   {"run.delta"},
	"action":  {"run.action"},
	"usage":   {"run.usage"},
	"context": {"run.context"},
	"control": {"control"},
}

func init() {
	// The lifecycle family has no single new prefix: its types sit directly
	// under "run".
	var lc []string
	for k, t := range kinds {
		if strings.HasPrefix(k, "lifecycle.") {
			lc = append(lc, t)
		}
	}
	slices.Sort(lc)
	families["lifecycle"] = lc
}

// TypeOf returns the type an old kind maps to. A dropped kind is ErrDropped
// and a kind this package does not know is ErrUnknown.
func TypeOf(kind string) (string, error) {
	if t, ok := kinds[kind]; ok {
		return t, nil
	}
	if slices.Contains(dropped, kind) {
		return "", fmt.Errorf("%w: %q", ErrDropped, kind)
	}
	return "", fmt.Errorf("%w: %q", ErrUnknown, kind)
}

// KindOf returns the old kind a type replaced, for a reader that still
// speaks the old wire shape. A type with no old kind reports false.
func KindOf(typ string) (string, bool) {
	for k, t := range kinds {
		if t == typ {
			return k, true
		}
	}
	return "", false
}

// Topic is the old topic a run event was published to: agent:<name>:<run>.
func Topic(agent, run string) string { return "agent:" + agent + ":" + run }

// KeptKinds returns the old kinds that still map to a type, sorted. Stored
// data depends on these names, so they never change.
func KeptKinds() []string {
	out := make([]string, 0, len(kinds))
	for k := range kinds {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// DroppedKinds returns the old kinds that map to no type, sorted.
func DroppedKinds() []string { return slices.Sorted(slices.Values(dropped)) }

// KindSelector returns the type prefixes an old route or filter selector
// chose: an exact kind ("usage.call") or a family ("delta"). An empty,
// dropped or unknown selector is an error, never an empty match.
func KindSelector(sel string) ([]string, error) {
	if t, ok := kinds[sel]; ok {
		return []string{t}, nil
	}
	if p, ok := families[sel]; ok {
		return slices.Clone(p), nil
	}
	if slices.Contains(dropped, sel) {
		return nil, fmt.Errorf("%w: selector %q", ErrDropped, sel)
	}
	return nil, fmt.Errorf("%w: selector %q", ErrUnknown, sel)
}

// KindSelectors maps a whole old selector list. An empty list selected every
// run event that is not opt-in, which is the prefix "run".
func KindSelectors(sels []string) ([]string, error) {
	if len(sels) == 0 {
		return []string{"run"}, nil
	}
	var out []string
	for _, s := range sels {
		p, err := KindSelector(s)
		if err != nil {
			return nil, err
		}
		for _, x := range p {
			if !slices.Contains(out, x) {
				out = append(out, x)
			}
		}
	}
	return out, nil
}

// oldEvent is the fields an old event shared across the bus row and the
// journal entry.
type oldEvent struct {
	Topic  string    `json:"topic"`
	RunID  string    `json:"run_id"`
	Tenant string    `json:"tenant"`
	Agent  string    `json:"agent"`
	Rev    int       `json:"rev"`
	Origin string    `json:"origin"`
	Kind   string    `json:"kind"`
	At     time.Time `json:"at"`
}

// DecodeBusRow reads one row of the old bus. Its payload is base64, as Go
// encoded the old []byte field. The row's sequence is stored beside the
// body, not in it.
func DecodeBusRow(seq uint64, body []byte) (event.Envelope, error) {
	var w struct {
		oldEvent
		Payload []byte `json:"payload"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		return event.Envelope{}, fmt.Errorf("legacy: bus row %d: %w", seq, err)
	}
	return fromOld(w.oldEvent, seq, w.Payload)
}

// DecodeEntry reads one old journal entry. The runtime's journal held the
// payload as inline JSON; a store that marshaled the old event whole held it
// as base64, a JSON string. Both are read.
func DecodeEntry(raw []byte) (event.Envelope, error) {
	var w struct {
		oldEvent
		Seq     uint64          `json:"seq"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		return event.Envelope{}, fmt.Errorf("legacy: entry: %w", err)
	}
	payload := []byte(w.Payload)
	if len(payload) > 0 && payload[0] == '"' {
		var b []byte
		if err := json.Unmarshal(payload, &b); err != nil {
			return event.Envelope{}, fmt.Errorf("legacy: entry %d: payload: %w", w.Seq, err)
		}
		payload = b
	}
	return fromOld(w.oldEvent, w.Seq, payload)
}

// RunChannel is the channel an old run event is read onto. Old events were
// keyed by agent and run, so the channel names both.
func RunChannel(tenant, agent, run string) string {
	if tenant == "" {
		tenant = "default"
	}
	return "fiber://" + tenant + "/agent/" + agent + "/" + run
}

// fromOld builds an envelope from an old event. The old lifecycle payload
// held every lifecycle field; decoding it as the body of the kind's own type
// keeps only that type's fields.
func fromOld(o oldEvent, seq uint64, payload []byte) (event.Envelope, error) {
	typ, err := TypeOf(o.Kind)
	if err != nil {
		return event.Envelope{}, err
	}
	if o.RunID == "" {
		return event.Envelope{}, fmt.Errorf("legacy: %s seq %d names no run", o.Kind, seq)
	}
	body, err := event.Decode(typ, payload)
	if err != nil {
		return event.Envelope{}, fmt.Errorf("legacy: %s seq %d: %w", o.Kind, seq, err)
	}
	e := event.Envelope{
		ID:      o.RunID + ":" + strconv.FormatUint(seq, 10),
		Source:  "fiber://run/" + o.RunID,
		Type:    typ,
		Channel: RunChannel(o.Tenant, o.Agent, o.RunID),
		Time:    o.At,
		Tenant:  o.Tenant,
		Run:     o.RunID,
		Seq:     seq,
		Agent:   o.Agent,
		Data:    body,
	}
	if o.Rev != 0 {
		e.Version = strconv.Itoa(o.Rev)
	}
	return e, nil
}
