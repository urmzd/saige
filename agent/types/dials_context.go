package types

import "context"

type dialPolicyKey struct{}

// ContextWithDialPolicy returns a context whose calls use p for dials,
// overriding the agent's own policy. Evals use it to hold settings constant
// across models.
func ContextWithDialPolicy(ctx context.Context, p DialPolicy) context.Context {
	return context.WithValue(ctx, dialPolicyKey{}, p.Clone())
}

// DialPolicyFromContext returns the policy ContextWithDialPolicy set.
func DialPolicyFromContext(ctx context.Context) (DialPolicy, bool) {
	p, ok := ctx.Value(dialPolicyKey{}).(DialPolicy)
	if !ok {
		return DialPolicy{}, false
	}
	return p.Clone(), true
}
