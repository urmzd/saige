package native

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	saigetypes "github.com/urmzd/saige/agent/types"

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

// Output is the JSON schema an agent's answer is constrained to: see
// turn.Output and responseSchema.
type Output = turn.Output

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

// responseSchema decodes the schema rev constrains its answer to, or returns
// nil when it constrains none: no Output, or an empty or null schema.
//
// saige's ParameterSchema carries the keywords a provider's structured output
// is built from: type, properties, required, items, enum, description,
// default and nullable. Any other JSON Schema keyword (pattern, minimum,
// format, additionalProperties, ...) is dropped here, which is safe because
// the host that set the schema validates the answer against the whole of it;
// the model is constrained to the shape. A schema that does not decode (not
// a JSON object, a property that is not a schema, a list of types at the
// top) is refused, and so is a top-level type other than "object". Neither
// is dropped: an agent that reads as answering in JSON and silently answers
// in prose is worse than one that does not start.
func responseSchema(r Revision) (*saigetypes.ParameterSchema, error) {
	if r.Spec.Output == nil {
		return nil, nil
	}
	raw := bytes.TrimSpace(r.Spec.Output.Schema)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	var s saigetypes.ParameterSchema
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("%w: agent %q version %s: output schema is not a JSON Schema saige can carry: %v",
			ErrInvalid, r.Spec.Name, r.Version, err)
	}
	if s.Type != saigetypes.SchemaObject {
		return nil, fmt.Errorf("%w: agent %q version %s: output schema has type %q, want %q",
			ErrInvalid, r.Spec.Name, r.Version, s.Type, saigetypes.SchemaObject)
	}
	return &s, nil
}
