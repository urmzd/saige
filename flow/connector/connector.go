// Package connector defines what it means to participate in the mesh.
//
// A connector is a peer, not a client. It may implement up to three faces,
// and which ones it implements is what gives the mesh its shape:
//
//	Trigger  publishes events (a Slack mention, a GitHub review request, a schedule)
//	Sink     renders events   (a Slack thread, a GitHub comment, a webhook POST)
//	Actor    contributes actions the agent can call (transition a Jira ticket)
//
// The faces are independent, and most connectors do not implement all three:
//
//	slack    trigger + sink + actor
//	github   trigger + sink + actor
//	jira     trigger + sink + actor
//	cron     trigger only          — a schedule has no surface to deliver to
//	webhook  sink only             — an outbound POST raises nothing
//
// Trigger and Sink are separate faces rather than two halves of one request.
// No connector ever calls another connector: triggers publish, sinks
// subscribe (pkg/egress), and the bus is the only thing in between.
//
// The Actor face is what turns "the agent replied" into "the work got done".
// Actions are the connector's native verbs, executed under the agent's policy
// rather than freely, and each one that runs emits an action.taken event so
// the work is auditable on the same bus as the conversation.
package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/event"
)

// Errors returned by connectors and by the registry.
var (
	// ErrUnsupported reports a face the connector does not implement.
	ErrUnsupported = errors.New("connector: unsupported")
	// ErrNotFound reports an unregistered connector name.
	ErrNotFound = errors.New("connector: not found")
	// ErrUndeliverable reports a target this connector cannot reach: a
	// different connector's scheme, or a workspace it is not configured for.
	ErrUndeliverable = errors.New("connector: undeliverable")
)

// Connector is the identity every face shares. Name is the address scheme
// this connector owns ("github" owns github:// targets).
type Connector interface {
	Name() string
}

// Publish puts one event on the bus. A trigger is handed one by whoever
// runs it, so it never learns what a bus or a runtime is.
type Publish func(ctx context.Context, e event.Envelope) error

// Trigger is an ingress adapter: it turns what happens on a surface into
// typed events and publishes them.
//
// Ingest runs until ctx is cancelled, publishing one event for every native
// event the surface has a type for. Implementations are responsible for
// verifying authenticity before publishing (HMAC for webhook-style triggers,
// socket authentication for streaming ones): nothing downstream re-checks,
// because nothing downstream can. A trigger never chooses an agent: the
// members of the event's channel decide who it wakes.
//
// Publishing the same event twice is safe. Its id is part of the run key, so
// a redelivery joins the existing run rather than starting a second one.
type Trigger interface {
	Connector
	Ingest(ctx context.Context, publish Publish) error
}

// Sink renders events onto a native surface.
//
// Deliver is called once per event its egress subscription selected. Implementations
// must tolerate redelivery of the same Event.Seq, because the bus is
// at-least-once: rendering should be idempotent, which for most surfaces
// means editing a message keyed by run rather than posting a new one.
//
// A sink that cannot reach target returns ErrUndeliverable rather than
// guessing.
type Sink interface {
	Connector
	Deliver(ctx context.Context, target address.Address, e event.Envelope) error
}

// Subscription is what an egress adapter receives: events of tenant runs on
// Channels (channel prefixes) whose type Types selects (type prefixes, with
// opt-in families only when named). Target is where they are rendered; zero
// renders each event on its own channel.
type Subscription struct {
	Channels []string
	Types    []string
	Target   address.Address
}

// Subscriber is a Sink that names its own subscriptions, such as an outbound
// webhook that receives another surface's channels. A Sink that is not one
// receives the run events on its own scheme's channels, rendered on each
// event's channel.
type Subscriber interface {
	Sink
	Subscriptions() []Subscription
}

// Actor contributes native verbs the agent can call as tools.
type Actor interface {
	Connector
	Actions() []Action
}

// Action is one native verb: create an issue, transition a ticket, request a
// review. It is described well enough for a model to call it and executed
// under the agent's policy.
type Action struct {
	// Name is the tool name the agent sees. Convention is
	// "<connector>_<verb>", such as "jira_transition".
	Name string
	// Description tells the model when to reach for this action.
	Description string
	// Schema is the JSON Schema for Args.
	Schema json.RawMessage
	// Invoke performs the action against target.
	Invoke func(ctx context.Context, target address.Address, args json.RawMessage) (Result, error)
}

// Result is the outcome of an action. Summary is what the agent reads back;
// Address points at whatever was created or changed, so the action.taken
// event can cite it.
type Result struct {
	Summary string          `json:"summary"`
	Address address.Address `json:"address,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Roles reports which faces a connector implements. It is derived, not
// declared, so a connector cannot advertise a face it does not have.
type Roles struct {
	Trigger bool
	Sink    bool
	Actor   bool
}

// RolesOf inspects c and reports the faces it implements.
func RolesOf(c Connector) Roles {
	_, trigger := c.(Trigger)
	_, sink := c.(Sink)
	_, actor := c.(Actor)
	return Roles{Trigger: trigger, Sink: sink, Actor: actor}
}

// ValidName reports whether s can name a connector.
//
// A connector name is stricter than the URI scheme it becomes. address
// .ValidScheme allows "." because URI does, but a connector name is also a
// single token everywhere else it travels: it is the family segment of an
// event kind ("github.issue.commented"), the prefix of every action an Actor
// contributes to an agent's tool catalog, and a subject token in the delivery
// family. "." separates tokens in all three, so a connector named "a.b" would
// be addressable and yet split in two the moment anything else handled its
// name.
//
// Rejecting it here — where the name is declared, at startup — turns that into
// a boot failure instead of a mis-parse in three unrelated places.
func ValidName(s string) error {
	if err := address.ValidScheme(s); err != nil {
		return fmt.Errorf("name %q is not address-scheme safe: %w", s, err)
	}
	for _, r := range s {
		if r == '.' || r == '+' {
			return fmt.Errorf("%w: connector name %q contains %q, which separates tokens in event kinds, tool names and delivery subjects", address.ErrInvalid, s, r)
		}
	}
	return nil
}

// Registry holds the connectors a deployment runs. It is populated once at
// startup and read concurrently thereafter.
type Registry struct {
	byName map[string]Connector
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byName: make(map[string]Connector)}
}

// Register adds c. A connector implementing none of the three faces is
// rejected: it could not participate, and registering it would only produce a
// confusing silence later.
func (r *Registry) Register(c Connector) error {
	if c == nil {
		return fmt.Errorf("connector: register nil")
	}
	name := c.Name()
	if name == "" {
		return fmt.Errorf("connector: register unnamed connector")
	}
	if err := ValidName(name); err != nil {
		return fmt.Errorf("connector: %w", err)
	}
	if roles := RolesOf(c); !roles.Trigger && !roles.Sink && !roles.Actor {
		return fmt.Errorf("%w: %q implements no connector face", ErrUnsupported, name)
	}
	if _, exists := r.byName[name]; exists {
		return fmt.Errorf("connector: %q already registered", name)
	}
	r.byName[name] = c
	return nil
}

// Get returns a connector by name.
func (r *Registry) Get(name string) (Connector, error) {
	c, ok := r.byName[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	return c, nil
}

// Sink returns the connector that owns target's scheme, if it can receive
// deliveries.
func (r *Registry) Sink(target address.Address) (Sink, error) {
	c, err := r.Get(target.Connector)
	if err != nil {
		return nil, err
	}
	s, ok := c.(Sink)
	if !ok {
		return nil, fmt.Errorf("%w: %q cannot receive deliveries", ErrUnsupported, target.Connector)
	}
	return s, nil
}

// Sinks returns every registered connector that renders events, in name
// order.
func (r *Registry) Sinks() []Sink {
	var out []Sink
	for _, name := range r.Names() {
		if s, ok := r.byName[name].(Sink); ok {
			out = append(out, s)
		}
	}
	return out
}

// Triggers returns every registered connector that publishes events.
func (r *Registry) Triggers() []Trigger {
	var out []Trigger
	for _, name := range r.Names() {
		if t, ok := r.byName[name].(Trigger); ok {
			out = append(out, t)
		}
	}
	return out
}

// Actions returns every action contributed by every registered Actor, which
// is the full catalog an agent spec may grant from.
func (r *Registry) Actions() []Action {
	var out []Action
	for _, name := range r.Names() {
		if a, ok := r.byName[name].(Actor); ok {
			out = append(out, a.Actions()...)
		}
	}
	return out
}

// Names returns the registered connector names in sorted order, so that
// iteration and any derived catalog are deterministic.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.byName))
	for name := range r.byName {
		out = append(out, name)
	}
	sortStrings(out)
	return out
}

// sortStrings is an insertion sort: registries hold a handful of connectors,
// and this keeps the package free of imports it would otherwise need only
// here.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
