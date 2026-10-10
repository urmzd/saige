// Package schemacheck builds and recognizes the error a provider returns when
// it cannot enforce a response schema. Decorators return it instead of
// dropping the schema, and fallback recognizes it so it can skip that member
// and try one that can enforce the schema.
package schemacheck

import (
	"errors"
	"fmt"

	"github.com/urmzd/saige/agent/types"
)

// unsupportedError matches types.ErrInvalidModelConfig and carries the same
// text as ModelCapabilities.OptionError, so callers that only check the
// sentinel see no difference.
type unsupportedError struct{ reason string }

func (e *unsupportedError) Error() string {
	return fmt.Sprintf("%v: structured_output: %s", types.ErrInvalidModelConfig, e.reason)
}

func (e *unsupportedError) Is(target error) bool {
	return target == types.ErrInvalidModelConfig || target == types.ErrSchemaUnsupported
}

// Unsupported reports that p cannot enforce a response schema. The error is
// permanent and matches types.ErrInvalidModelConfig.
func Unsupported(p types.Provider, reason string) error {
	caps, _ := types.ProviderCapabilities(p)
	if caps.Provider == "" {
		caps.Provider = types.NameOf(p)
	}
	if caps.Model == "" {
		caps.Model = types.ProviderModel(p)
	}
	return &types.ProviderError{Provider: caps.Provider, Model: caps.Model,
		Kind: types.ErrorKindPermanent, Err: &unsupportedError{reason: reason}}
}

// IsUnsupported reports whether err says a provider cannot enforce a response
// schema. Such a request never reached the network, so another provider can
// serve it.
func IsUnsupported(err error) bool {
	return errors.Is(err, types.ErrSchemaUnsupported)
}
