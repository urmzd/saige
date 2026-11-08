// Package connectors holds the mesh: the adapters that turn surfaces into
// typed events and render run events back onto them.
//
// Each subpackage implements the faces of pkg/connector that its surface can
// actually support, and the asymmetry between them is the whole point:
//
//	connector  ingress                      egress            actor
//	slack      message.posted.v1,           thread messages   yes
//	           slack.command.invoked.v1,
//	           slack.session.stopped.v1
//	github     github.<event>.<action>.v1   comments, checks  yes
//	jira       jira.*.v1                    comments          yes
//	cron       cron.tick.v1                 no                no
//	webhook    no                           envelope POST v2  no
//	author     no                           no                yes
//
// A schedule has no surface to deliver to, an outbound POST raises nothing,
// and author reaches inward: its actions read and change the tenant's agent
// definitions (definition.Source) for an agent granted them.
//
// mention is a helper, not a connector: it finds the agents a text names and
// strips the mention, and reads the people, places and links it refers to,
// for every ingress adapter. References converts what it reads into the
// event's own type.
//
// An ingress adapter verifies a request, publishes one typed event on the
// surface's channel with the agents it mentions and the references it makes
// (event.Request.References), and chooses no agent: the channel's members do
// (pkg/membership, pkg/dispatch). An egress adapter
// subscribes by channel and type prefix (pkg/egress) and renders what
// arrives. No connector imports another. An event raised in github reaches
// jira because both talk to the bus, never to each other, and
// pkg/connector.RolesOf derives the faces from the types rather than trusting
// a declaration.
//
// # Two rules shared by every human-facing sink
//
// Delivery is at-least-once, so rendering must be idempotent. The shape that
// achieves this is one message per (run, target) that gets EDITED as the run
// proceeds, never a new message per delta: a Slack channel that got one message
// per token would be unusable, and a redelivered event would double every line.
// The engine in internal/sink owns that invariant, and every event is rendered
// by re-rendering the whole accumulated document, which makes redelivery a
// no-op by construction rather than by bookkeeping.
//
// Ordering is not guaranteed either, so accumulation is order-insensitive.
// internal/render files each event under its Envelope.Seq, so a late low-Seq
// text delta lands in its correct position instead of appending stale text
// after fresh text, and a Seq already applied is dropped.
//
// The exception proves the rule: the webhook sink posts every event, because
// its receiver is a program that dedupes on the delivery key rather than a
// human reading a thread.
//
// # No provider SDKs
//
// These connectors speak to their APIs with net/http and encoding/json. A
// handful of endpoints per surface does not justify pulling a vendor SDK (and
// its transitive graph) into a module whose reason to exist is being the bus.
// Every connector puts its calls behind a small interface with a real
// implementation and an in-memory fake, so all the logic above the wire
// (mention parsing, addressing, idempotent rendering, coalescing, retries) is
// tested without a network.
package connectors
