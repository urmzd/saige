package tree

import (
	"encoding/json"
	"errors"
)

// WithMetadata attaches an immutable JSON object to the conversation tree.
// Consumers can include their run specification, revision, and effective
// configuration. Do not include provider credentials or transport headers.
// New and FromStore reject metadata that is not a valid JSON object.
func WithMetadata(metadata json.RawMessage) Option {
	owned := append(json.RawMessage(nil), metadata...)
	return func(t *Tree) { t.metadata = append(json.RawMessage(nil), owned...) }
}

func validateMetadata(metadata json.RawMessage) error {
	if len(metadata) == 0 {
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &object); err != nil || object == nil {
		return errors.New("tree: metadata must be a JSON object")
	}
	return nil
}
