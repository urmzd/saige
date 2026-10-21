package fallback

import (
	"fmt"
	"slices"
	"strings"

	"github.com/urmzd/saige/agent/types"
)

// Conditions a fallback rule may name: the classes of model error that move
// a call on to the next member, each one of package types' error
// classifications. These are the names a configuration writes, so they are
// part of its contract: never rename one. Whatever is named, an error
// DefaultFallbackOn stops on (a cancelled call, an invalid request or
// configuration, an exhausted budget) never falls through.
const (
	// OnAny falls through on every error DefaultFallbackOn passes on. It
	// stands alone, and it is what naming no condition means.
	OnAny = "any"
	// OnUnavailable is an outage, types.IsUnavailable: a 5xx, an overloaded
	// provider.
	OnUnavailable = "unavailable"
	// OnRateLimit is a rate limit or an exhausted quota, types.IsRateLimit.
	OnRateLimit = "rate_limit"
	// OnTransient is any error a retry could fix, types.IsTransient: a
	// timeout, a reset connection, and also a rate limit or an outage.
	OnTransient = "transient"
	// OnAuth is a refused or missing credential, types.IsAuth.
	OnAuth = "auth"
	// OnContextLength is a prompt too long for the model,
	// types.IsContextLength.
	OnContextLength = "context_length"
	// OnContentFilter is a request or response a provider's safety system
	// blocked, types.IsContentFilter.
	OnContentFilter = "content_filter"
)

// conditionTests maps each condition but OnAny to its classification.
var conditionTests = map[string]func(error) bool{
	OnUnavailable:   types.IsUnavailable,
	OnRateLimit:     types.IsRateLimit,
	OnTransient:     types.IsTransient,
	OnAuth:          types.IsAuth,
	OnContextLength: types.IsContextLength,
	OnContentFilter: types.IsContentFilter,
}

// Conditions returns every condition a fallback rule may name, sorted. It
// returns a fresh slice, so a caller cannot edit this package's answer.
func Conditions() []string {
	out := []string{OnAny}
	for c := range conditionTests {
		out = append(out, c)
	}
	slices.Sort(out)
	return out
}

// ConditionTest returns the classification a condition names, and whether
// it names one. OnAny's is DefaultFallbackOn; the others are package types'
// tests alone, without DefaultFallbackOn's exclusions (Rule adds them).
func ConditionTest(condition string) (func(error) bool, bool) {
	if condition == OnAny {
		return DefaultFallbackOn, true
	}
	test, ok := conditionTests[condition]
	return test, ok
}

// Rule returns the Config.FallbackOn predicate for the named conditions. No
// conditions, or OnAny alone, is DefaultFallbackOn. Named classes narrow
// that default and never widen it: an error falls through when
// DefaultFallbackOn would pass it on and it is of a named class, so a
// cancelled call, an invalid request or an exhausted budget never does. A
// condition named twice counts once.
//
// An unknown condition, or OnAny with another, is an error wrapping
// types.ErrInvalidConfig that lists the conditions: ignoring it would fall
// back more or less often than the configuration says.
func Rule(conditions ...string) (func(error) bool, error) {
	if len(conditions) == 0 {
		return DefaultFallbackOn, nil
	}
	if slices.Contains(conditions, OnAny) {
		if others := otherConditions(conditions); len(others) > 0 {
			return nil, fmt.Errorf("%w: fallback: condition %q stands alone, and already covers %s",
				types.ErrInvalidConfig, OnAny, strings.Join(others, ", "))
		}
		return DefaultFallbackOn, nil
	}
	tests := make([]func(error) bool, 0, len(conditions))
	seen := make(map[string]bool, len(conditions))
	for _, c := range conditions {
		test, ok := conditionTests[c]
		if !ok {
			return nil, fmt.Errorf("%w: fallback: unknown condition %q; the conditions are %s (%q alone)",
				types.ErrInvalidConfig, c, strings.Join(Conditions(), ", "), OnAny)
		}
		if !seen[c] {
			seen[c] = true
			tests = append(tests, test)
		}
	}
	return func(err error) bool {
		if !DefaultFallbackOn(err) {
			return false
		}
		for _, test := range tests {
			if test(err) {
				return true
			}
		}
		return false
	}, nil
}

func otherConditions(conditions []string) []string {
	var out []string
	for _, c := range conditions {
		if c != OnAny && !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out
}
