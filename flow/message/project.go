package message

import (
	"encoding/json"

	"github.com/urmzd/saige/flow/event"
)

// Option configures a Projector.
type Option func(*options)

type options struct {
	toolUseID  func(run string, n int) string
	classify   func(name string, args json.RawMessage) (kind, title string)
	background func(name string) bool
}

// WithToolUseID sets how a run's n-th tool call is named in the stream.
// The default is ToolUseID. A host that mints its own ids passes the same
// rule it stamps ParentToolCall with, so a called run nests under its call.
func WithToolUseID(fn func(run string, n int) string) Option {
	return func(o *options) { o.toolUseID = fn }
}

// WithClassify sets a tool call's kind and card title from its name and
// arguments. The default: "agent" is an agent call titled by its "title"
// argument, "get_context" is a context pull, and anything else a tool.
func WithClassify(fn func(name string, args json.RawMessage) (kind, title string)) Option {
	return func(o *options) { o.classify = fn }
}

// WithBackground names the tools whose calls run in the background: the
// response ends without waiting for the run they start. The default is
// "delegate_task".
func WithBackground(fn func(name string) bool) Option {
	return func(o *options) { o.background = fn }
}

// Projector writes one response's stream from the envelopes of its run
// tree. It is not safe for concurrent use.
type Projector struct {
	head Head
	opts options
}

// NewProjector returns a projector for the response head describes, whose
// root run is head.RootRun.
func NewProjector(head Head, opts ...Option) *Projector {
	p := &Projector{head: head}
	for _, o := range opts {
		o(&p.opts)
	}
	return p
}

// Add projects one envelope of the tree, given in total order (Less), and
// returns the events it adds to the stream. An envelope of another tree, or
// one already projected (by its run and sequence), adds nothing.
func (p *Projector) Add(e event.Envelope) []Event {
	return nil
}

// Cursor is how far the projector has read.
func (p *Projector) Cursor() Cursor { return Cursor{} }

// Done reports whether the stream has ended: message_stop was sent.
func (p *Projector) Done() bool { return false }
