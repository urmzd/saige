package native

import (
	"errors"
	"fmt"

	"github.com/urmzd/saige/flow/turn"
)

// ErrInvalid reports a revision this package cannot build an agent from.
// Callers match with errors.Is.
var ErrInvalid = errors.New("native: invalid")

// Spec is what an agent is: see turn.Spec. A Spec is immutable: editing an
// agent makes the next Revision.
type Spec = turn.Spec

// ModelRef names one model behind one provider: one of a spec's fallbacks.
type ModelRef = turn.ModelRef

// Source is one retrieval corpus the agent may ground answers in.
type Source = turn.Source

// Access is what the agent may do beyond talking: see turn.Policy.
type Access = turn.Policy

// Revision is the pinned definition a Runner executes: a name, an exact
// version, the digest it was pinned with, and the compiled spec.
type Revision = turn.Pinned

// validate checks the fields this package needs to construct an agent. It
// does not resolve tools or sources: those depend on what a deployment has
// registered.
func validate(r Revision) error {
	if err := r.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if r.Spec.Provider == "" {
		return fmt.Errorf("%w: agent %q version %s names no provider", ErrInvalid, r.Spec.Name, r.Version)
	}
	return nil
}
