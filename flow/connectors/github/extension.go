package github

import (
	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connectors/internal/render"
	"github.com/urmzd/saige/flow/connectors/internal/sink"
)

// Hook is the decoded webhook union. Automation callbacks receive it only
// after signature verification and repository validation.
type Hook = hook

// View is an immutable snapshot of a run being delivered.
type View = render.View

const (
	StatusRunning = render.StatusRunning
	StatusDone    = render.StatusDone
)

// RenderDefault renders ordinary Markdown with lifecycle progress.
func RenderDefault(v View) string {
	return sink.Clamp(sink.Layout(markdown{}, v), MaxBodyChars)
}

// Reference validates the connector's owner boundary and decodes an address.
func (c *Connector) Reference(a address.Address) (Ref, error) { return c.ref(a) }

// ValidSHA reports whether a reference is a full hexadecimal commit ID.
func ValidSHA(s string) bool { return validSHA(s) }

// ValidateChanges checks bounded repository-relative UTF-8 replacements.
func ValidateChanges(c ChangeRequest) error { return validateChanges(c) }
