package convert

import (
	"context"

	"github.com/urmzd/saige/agent/types"
)

// Runtime is what the caller of a provider supplies to the conversions an
// attempt runs: a policy above the decorator's own, the modality dial layers
// of its scopes, and the durable runner and budget the conversions are
// recorded and charged under. The agent loop sets it for every model call.
type Runtime struct {
	// Policy applies on top of the decorator's policy: its dial above, its
	// converters first, its cache, cap, scope and thinking rule when set.
	Policy types.ConversionPolicy
	// Layers are modality dial layers above the decorator's own, such as
	// the agent's and the turn's.
	Layers []types.DialLayer
	// Steps runs each conversion as a durable step, so a replay never runs
	// (or bills) it again. Nil runs conversions inline.
	Steps types.StepRunner
	// Budget is charged for converters that call a model. Reservation is
	// the attempt's reservation the charge is carved from (see
	// types.Budget.Carve); empty reserves on its own.
	Budget      *types.Budget
	Reservation string
	// OnReceipt receives the settlement of every conversion charged to
	// Budget, including one restored on replay.
	OnReceipt func(types.BudgetReceipt)
}

type runtimeKey struct{}

type nestedKey struct{}

// WithRuntime returns ctx carrying rt for the conversion decorators of the
// providers called with it.
func WithRuntime(ctx context.Context, rt Runtime) context.Context {
	return context.WithValue(ctx, runtimeKey{}, rt)
}

// RuntimeFrom returns the runtime ctx carries.
func RuntimeFrom(ctx context.Context) (Runtime, bool) {
	rt, ok := ctx.Value(runtimeKey{}).(Runtime)
	return rt, ok
}

// converterContext is the context a converter runs under: the caller's
// runtime is removed, so a converter's own model call is not converted with
// the policy of the request it serves, and a decorator around that model
// passes its request through unplanned instead of recursing.
func converterContext(ctx context.Context) context.Context {
	ctx = context.WithValue(ctx, runtimeKey{}, nil)
	return context.WithValue(ctx, nestedKey{}, true)
}

// nested reports whether ctx is a converter's own model call.
func nested(ctx context.Context) bool {
	v, _ := ctx.Value(nestedKey{}).(bool)
	return v
}
