// Package wrapper defines how provider decorators expose the providers they
// wrap, so a caller can find an optional interface behind any stack of
// decorators.
//
// A decorator around one provider implements Wrapper. A decorator over several
// providers, such as a fallback chain, a router, or a traffic split,
// implements MultiWrapper and returns its members in priority order. This
// follows the errors.Unwrap convention: As checks the provider itself first,
// then walks the chain depth first.
//
// Unwrapping is for discovery, not for bypassing a decorator. A decorator that
// changes behavior (retry, caching, routing) still forwards the optional
// interfaces the agent loop relies on directly, so a type assertion on the
// outermost provider keeps working. Use As for interfaces that describe a
// specific member, such as a bound provider-side cache handle.
package wrapper

import "github.com/urmzd/saige/agent/types"

// maxDepth bounds the walk so a decorator that returns itself cannot loop.
const maxDepth = 64

// Wrapper is implemented by a decorator around exactly one provider.
type Wrapper interface {
	Unwrap() types.Provider
}

// MultiWrapper is implemented by a decorator over several providers. Members
// are returned in the order the decorator prefers them.
type MultiWrapper interface {
	Unwrap() []types.Provider
}

// Unwrap returns the provider p wraps, or nil when p is not a single-provider
// decorator.
func Unwrap(p types.Provider) types.Provider {
	if w, ok := p.(Wrapper); ok {
		return w.Unwrap()
	}
	return nil
}

// Members returns the providers p wraps directly: one for a Wrapper, every
// member for a MultiWrapper, and nil for a provider that wraps nothing.
func Members(p types.Provider) []types.Provider {
	switch w := p.(type) {
	case Wrapper:
		if inner := w.Unwrap(); inner != nil {
			return []types.Provider{inner}
		}
	case MultiWrapper:
		return w.Unwrap()
	}
	return nil
}

// Walk calls fn on p and then on every provider it wraps, depth first in
// member order. It stops when fn returns false.
func Walk(p types.Provider, fn func(types.Provider) bool) {
	walk(p, fn, 0)
}

func walk(p types.Provider, fn func(types.Provider) bool, depth int) bool {
	if p == nil || depth > maxDepth {
		return true
	}
	if !fn(p) {
		return false
	}
	for _, m := range Members(p) {
		if !walk(m, fn, depth+1) {
			return false
		}
	}
	return true
}

// As finds the first provider in p's chain that implements T, checking p
// itself first. It reports false when no provider in the chain implements T.
func As[T any](p types.Provider) (T, bool) {
	var found T
	ok := false
	Walk(p, func(q types.Provider) bool {
		if t, match := q.(T); match {
			found, ok = t, true
			return false
		}
		return true
	})
	return found, ok
}

// Innermost follows single-provider decorators to the provider at the bottom
// of the chain. It stops at a MultiWrapper, because no single member stands
// for the whole set.
func Innermost(p types.Provider) types.Provider {
	for range maxDepth {
		inner := Unwrap(p)
		if inner == nil {
			return p
		}
		p = inner
	}
	return p
}

// InnermostName is the name of the provider at the bottom of p's decorator
// chain: "openai" for a retry-wrapped OpenAI adapter, not "retry(openai)".
// Route events and traces report it, since it names the vendor that served.
// A multi-provider decorator reports its own name.
func InnermostName(p types.Provider) string {
	return types.ProviderName(Innermost(p))
}
