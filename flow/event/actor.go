package event

import "context"

// Actor is the person or system that raised an event.
type Actor struct {
	// ID is the surface's own identifier (a Slack user ID, a GitHub login).
	// Stable within a workspace.
	ID string `json:"id"`
	// Display is a human-readable name, best effort.
	Display string `json:"display,omitempty"`
	// Bot reports whether the actor is automation rather than a person.
	Bot bool `json:"bot,omitempty"`
	// User is the host's own account for the actor, when the surface knows
	// it. It is an authorization fact like ID: flow trusts it as given, so a
	// program that accepts events from callers keeps it only from a
	// credential trusted to act for others.
	User string `json:"user,omitempty"`
}

// actorKey is the context key the actor a piece of work acts for travels
// under. It is unexported, so only WithActor sets it.
type actorKey struct{}

// WithActor returns ctx carrying a as the actor the work under it acts for.
// The host sets it from the run's durable record, which a model cannot
// rewrite. Code that acts on a person's behalf reads it with ActorFrom. A
// zero actor clears whatever ctx carried, so work that acts for nobody
// cannot inherit someone.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

// ActorFrom returns the actor ctx acts for. ok is false when it acts for
// nobody: no actor was set, or the one set names no one (no ID and no User).
func ActorFrom(ctx context.Context) (a Actor, ok bool) {
	a, _ = ctx.Value(actorKey{}).(Actor)
	if a.ID == "" && a.User == "" {
		return Actor{}, false
	}
	return a, true
}
