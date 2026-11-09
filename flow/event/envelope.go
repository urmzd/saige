package event

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrInvalid reports a malformed envelope. Callers match with errors.Is.
var ErrInvalid = errors.New("event: invalid")

// Private is host data that travels with an event inside the process and is
// never serialized: a Slack bot user id, a GitHub installation id. It is not
// rendered to a model, not stored as data and not sent to an egress adapter.
type Private map[string]string

// Envelope is one event. Its fields follow CloudEvents naming. Type is the
// only discriminator: it names the body Data holds.
type Envelope struct {
	// ID is unique per source. A redelivery keeps it.
	ID string `json:"id"`
	// Source is what produced the event, as a URI.
	Source string `json:"source"`
	// Type names the body and carries its version.
	Type string `json:"type"`
	// Subject is the resource inside Source, such as "issues/42".
	Subject string `json:"subject,omitempty"`
	// Channel is where the event is published, as a URI.
	Channel string `json:"channel"`
	// Time is when the source says the event happened.
	Time time.Time `json:"time"`
	// Hops is how many events led to this one.
	Hops int `json:"hops"`

	// Run-scoped fields, set on every run.* and control.* event. Tenant is
	// set by the host from the principal, never read from the wire.
	Tenant  string `json:"tenant,omitempty"`
	Run     string `json:"run,omitempty"`
	Seq     uint64 `json:"seq,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Version string `json:"version,omitempty"`
	Digest  string `json:"digest,omitempty"`
	// Session identifies the stable conversation, independently of a run.
	Session       string `json:"session,omitempty"`
	ParentSession string `json:"parent_session,omitempty"`

	// Lineage of a run in a tree of runs, stamped on every event of the run
	// when the run is created. Root is the run the tree answers from: a run
	// that no other run called is its own root. ParentRun is the run that
	// called this one, and ParentToolCall the id of the tool call it was
	// called through, as the host minted it. A subscriber that follows one
	// response subscribes by Root and nests each run under its parent call.
	Root           string `json:"root,omitempty"`
	ParentRun      string `json:"parent_run,omitempty"`
	ParentToolCall string `json:"parent_tool_call,omitempty"`

	// Data is the body for Type.
	Data Body `json:"data"`

	// Private is never serialized.
	Private Private `json:"-"`
}

// runScoped reports whether a type belongs to a run.
func runScoped(t string) bool {
	f := Family(t)
	return f == "run" || f == "control"
}

// Validate checks that the envelope can be published.
func (e Envelope) Validate() error {
	switch {
	case e.ID == "":
		return fmt.Errorf("%w: no id", ErrInvalid)
	case e.Source == "":
		return fmt.Errorf("%w: no source", ErrInvalid)
	case !ValidType(e.Type):
		return fmt.Errorf("%w: type %q is outside the grammar", ErrInvalid, e.Type)
	case e.Channel == "":
		return fmt.Errorf("%w: no channel", ErrInvalid)
	case e.Time.IsZero():
		return fmt.Errorf("%w: no time", ErrInvalid)
	case e.Hops < 0:
		return fmt.Errorf("%w: negative hops", ErrInvalid)
	case e.Data == nil:
		return fmt.Errorf("%w: no data", ErrInvalid)
	case e.Data.Type() != e.Type:
		return fmt.Errorf("%w: type %q carries a %q body", ErrInvalid, e.Type, e.Data.Type())
	case runScoped(e.Type) && e.Run == "":
		return fmt.Errorf("%w: %s names no run", ErrInvalid, e.Type)
	}
	if _, known := Lookup(e.Type); !known {
		if _, ok := e.Data.(Unknown); !ok {
			return fmt.Errorf("%w: unregistered type %q with a typed body", ErrInvalid, e.Type)
		}
	}
	return nil
}

// UnmarshalJSON reads type first and decodes data with the catalog entry for
// it. An unregistered type decodes to Unknown. A registered type whose data
// does not decode is an error: a broken known body is never passed on.
func (e *Envelope) UnmarshalJSON(b []byte) error {
	type plain Envelope
	var w struct {
		plain
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	if w.Type == "" {
		return fmt.Errorf("%w: no type", ErrInvalid)
	}
	body, err := Decode(w.Type, w.Data)
	if err != nil {
		return err
	}
	*e = Envelope(w.plain)
	e.Data = body
	return nil
}
