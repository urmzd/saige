package wrapper

import (
	"context"
	"fmt"

	"github.com/urmzd/saige/agent/provider/internal/optionscheck"
	"github.com/urmzd/saige/agent/provider/internal/schemacheck"
	"github.com/urmzd/saige/agent/types"
)

// Base is the embeddable part of a decorator around one provider. It
// forwards every optional interface the agent loop finds by a direct type
// assertion, so a decorator that embeds it overrides only what it changes:
//
//   - types.NamedProvider and types.ModelProvider report the inner
//     provider's name and model;
//   - types.CapabilityReporter reports the inner provider's capabilities,
//     without the option-only controls when it cannot receive options;
//   - types.OptionsReporter reports the options the inner provider sends;
//   - types.StructuredOutputProvider and types.OptionsProvider report true,
//     and Stream rejects a schema or options the inner provider cannot take
//     instead of dropping them;
//   - types.TargetSwitcher and types.SessionProvider re-target or isolate
//     the inner provider and rebuild the decorator around it with Rewrap;
//   - types.Closer closes the inner provider, which the decorator owns
//     (see NoClose);
//   - Wrapper exposes the inner provider to As and Walk.
//
// Interfaces that describe one member, such as an adapter's offering or
// dial surface, are not forwarded: find them with As.
type Base struct {
	// Inner is the decorated provider.
	Inner types.Provider
	// Rewrap returns the decorator around another inner provider, keeping
	// its own configuration. WithTarget and NewSession use it, so a
	// re-targeted or isolated provider keeps the decorator. A nil Rewrap
	// makes WithTarget fail and NewSession return the inner provider's
	// session without the decorator.
	Rewrap func(inner types.Provider) types.Provider
}

// NewBase returns a Base around inner that rebuilds its decorator with
// rewrap.
func NewBase(inner types.Provider, rewrap func(types.Provider) types.Provider) Base {
	return Base{Inner: inner, Rewrap: rewrap}
}

var (
	_ types.Provider                 = Base{}
	_ types.NamedProvider            = Base{}
	_ types.ModelProvider            = Base{}
	_ types.CapabilityReporter       = Base{}
	_ types.OptionsReporter          = Base{}
	_ types.StructuredOutputProvider = Base{}
	_ types.OptionsProvider          = Base{}
	_ types.TargetSwitcher           = Base{}
	_ types.SessionProvider          = Base{}
	_ types.Closer                   = Base{}
	_ Wrapper                        = Base{}
)

// Unwrap returns the inner provider.
func (b Base) Unwrap() types.Provider { return b.Inner }

// Name reports the inner provider's name.
func (b Base) Name() string { return types.NameOf(b.Inner) }

// Model reports the inner provider's model.
func (b Base) Model() string { return types.ProviderModel(b.Inner) }

// Capabilities reports the inner provider's capabilities. When the inner
// provider reports none, the zero value (Known false) is returned, so a
// caller that must fail closed still can. Capabilities that need request
// options are dropped when the inner provider cannot receive them.
func (b Base) Capabilities() types.ModelCapabilities {
	caps, _ := types.ProviderCapabilities(b.Inner)
	return optionscheck.Narrow(caps, b.Inner)
}

// EffectiveOptions reports the options the inner provider sends.
func (b Base) EffectiveOptions() types.RequestOptions {
	o, _ := types.ProviderEffectiveOptions(b.Inner)
	return o
}

// SupportsSchema reports true. A schema the inner provider cannot enforce
// is rejected by Check when the request arrives.
func (b Base) SupportsSchema() bool { return true }

// SupportsOptions reports true. Options the inner provider cannot receive
// are rejected by Check when the request arrives.
func (b Base) SupportsOptions() bool { return true }

// Check rejects a request carrying a schema or options the inner provider
// cannot receive, with an error matching types.ErrInvalidModelConfig. The
// caller asked for them and must not get output made without them. A
// decorator that overrides Stream calls it first.
func (b Base) Check(req types.Request) error {
	if req.Schema != nil && !types.AcceptsSchema(b.Inner) {
		return schemacheck.Unsupported(b.Inner, "provider cannot enforce a response schema")
	}
	if req.Options != nil && !types.AcceptsOptions(b.Inner) {
		return optionscheck.Unsupported(b.Inner)
	}
	return nil
}

// Stream checks req and sends it to the inner provider.
func (b Base) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	if err := b.Check(req); err != nil {
		return nil, err
	}
	return b.Inner.Stream(ctx, req)
}

// WithTarget re-targets the inner provider and rebuilds the decorator
// around the result.
func (b Base) WithTarget(t types.Target) (types.Provider, error) {
	if b.Rewrap == nil {
		return nil, fmt.Errorf("%w %s: decorator over %s cannot be re-targeted", types.ErrUnknownTarget, t, types.NameOf(b.Inner))
	}
	inner, err := types.ProviderWithTarget(b.Inner, t)
	if err != nil {
		return nil, err
	}
	return b.Rewrap(inner), nil
}

// NewSession isolates the inner provider's routing state and rebuilds the
// decorator around the session.
func (b Base) NewSession() types.Provider {
	inner := types.NewProviderSession(b.Inner)
	if b.Rewrap == nil {
		return inner
	}
	return b.Rewrap(inner)
}

// Close closes the inner provider.
func (b Base) Close(ctx context.Context) error { return types.CloseProvider(ctx, b.Inner) }

// NoClose returns p behind a decorator whose Close does nothing, for a
// provider shared by several owners: a decorator, a fallback chain or a
// router closes the providers it was given, so hand each owner but one a
// NoClose view.
func NoClose(p types.Provider) types.Provider {
	return noClose{Base: NewBase(p, NoClose)}
}

type noClose struct{ Base }

// Close does nothing: the provider has another owner.
func (noClose) Close(context.Context) error { return nil }
