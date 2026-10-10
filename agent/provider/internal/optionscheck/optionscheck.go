// Package optionscheck builds and recognizes the error a decorator returns
// when the provider it wraps cannot receive per-request options. Decorators
// return it instead of dropping the options, because a dropped tool choice
// changes what the model is allowed to do. Fallback recognizes it so it can
// skip that member and try one that accepts the options.
package optionscheck

import (
	"errors"
	"fmt"

	"github.com/urmzd/saige/agent/types"
)

// unsupportedError matches types.ErrInvalidModelConfig.
type unsupportedError struct{ provider string }

func (e *unsupportedError) Error() string {
	return fmt.Sprintf("%v: provider %q does not accept request options", types.ErrInvalidModelConfig, e.provider)
}

func (e *unsupportedError) Is(target error) bool {
	return target == types.ErrInvalidModelConfig || target == types.ErrOptionsUnsupported
}

// Unsupported reports that p does not accept request options. The
// error is permanent and matches types.ErrInvalidModelConfig.
func Unsupported(p types.Provider) error {
	return &types.ProviderError{
		Provider: types.NameOf(p),
		Model:    types.ProviderModel(p),
		Kind:     types.ErrorKindPermanent,
		Err:      &unsupportedError{provider: types.NameOf(p)},
	}
}

// IsUnsupported reports whether err says a provider cannot receive request
// options. Such a request never reached the network.
func IsUnsupported(err error) bool {
	return errors.Is(err, types.ErrOptionsUnsupported)
}

// optionOnly lists the capabilities a caller can exercise only through
// request options. A model may support them while the provider in front of it
// has no way to receive the options, so they are part of what a decorator
// reports only when the options can actually reach the model.
var optionOnly = []types.Capability{types.CapToolChoice, types.CapParallelToolControl}

// Narrow returns caps without the option-only capabilities when no provider
// in members implements types.OptionsProvider. A decorator always implements
// types.OptionsProvider itself, so a caller that asks "does this provider
// accept options and declare tool choice?" gets a true answer only when some
// member really does. Otherwise the caller withholds the tools to forbid tool
// use and rejects a forced choice up front, instead of sending options that
// fail at call time.
func Narrow(caps types.ModelCapabilities, members ...types.Provider) types.ModelCapabilities {
	for _, p := range members {
		if types.AcceptsOptions(p) {
			return caps
		}
	}
	return caps.Without(optionOnly...)
}
