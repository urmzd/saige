// Package event is the one shape every event takes: an Envelope whose Data is
// a typed Body, chosen by the envelope's Type.
//
// Type is the only discriminator, and it carries the version
// ("run.completed.v1"). Every known type is registered once in the catalog
// (catalog.go) with its decoder, a renderer that turns the body into model
// input, and a sensitivity flag. A type this build does not know decodes to
// Unknown, which keeps its bytes.
//
// See docs/architecture/events.md.
package event

// Body is the typed data of one event type. It is sealed: only this package
// defines bodies, so a type switch over Body is closed apart from Unknown.
type Body interface {
	// Type is the catalog type this body is the data of.
	Type() string
	isBody()
}
