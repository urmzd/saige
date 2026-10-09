package schemacheck

import (
	"errors"
	"fmt"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

type namedProvider struct{ types.Provider }

func (namedProvider) Name() string { return "plain" }

func TestUnsupported(t *testing.T) {
	err := Unsupported(namedProvider{}, "provider cannot enforce a response schema")
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"direct", err, true},
		{"wrapped", fmt.Errorf("member 0: %w", err), true},
		{"in fallback error", &types.FallbackError{Errors: []error{err}}, true},
		{"other invalid config", types.ModelCapabilities{}.OptionError("tool_choice", "x"), false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsUnsupported(tc.err); got != tc.want {
				t.Fatalf("IsUnsupported = %v, want %v", got, tc.want)
			}
		})
	}
	if !errors.Is(err, types.ErrInvalidModelConfig) || types.KindOf(err) != types.ErrorKindPermanent {
		t.Fatalf("err = %v, want a permanent ErrInvalidModelConfig", err)
	}
	var pe *types.ProviderError
	if !errors.As(err, &pe) || pe.Provider != "plain" {
		t.Fatalf("provider name not recorded: %v", err)
	}
}
