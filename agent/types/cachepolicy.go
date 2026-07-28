package types

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// CacheScope decides who may see a cached entry. Getting this wrong is the
// worst failure mode caching has: too wide and one tenant reads another's
// results, too narrow and the cache never hits.
type CacheScope string

const (
	// CacheScopeGlobal shares entries across everything. Correct only for
	// results that depend on nothing but the arguments: a unit conversion, a
	// public document fetch.
	CacheScopeGlobal CacheScope = "global"
	// CacheScopeSession confines entries to one conversation. The safe default
	// for anything that reads mutable state.
	CacheScopeSession CacheScope = "session"
	// CacheScopeAgent confines entries to one agent, so a sub-agent does not
	// read its parent's cached view.
	CacheScopeAgent CacheScope = "agent"
	// CacheScopeUser confines entries to one end user. Required for anything
	// touching per-user data or per-user credentials.
	CacheScopeUser CacheScope = "user"
)

// CachePolicy declares how a tool's results may be cached.
//
// It is a declaration rather than a switch because the interesting decisions
// are not "on or off". Which arguments identify a result, which context knobs
// change it, how long it stays true, whether a stale answer beats an error, and
// whether failures are cacheable at all are separate questions, and a tool
// author knows them while the deployment wiring the cache does not.
//
// The zero value caches nothing, so a tool that says nothing is never cached.
type CachePolicy struct {
	// Enabled turns caching on. Everything else is inert while it is false.
	Enabled bool

	// TTL is how long an entry stays fresh. Zero with Enabled set is rejected
	// by Validate: an unbounded tool cache is a memory leak that also serves
	// arbitrarily stale data, and neither failure announces itself.
	TTL time.Duration

	// Scope decides who shares entries. Empty defaults to CacheScopeSession,
	// the conservative choice.
	Scope CacheScope

	// KeyArgs names the arguments that participate in the key. Empty means all
	// of them. Naming them explicitly is what makes a cache hit when the model
	// passes a cosmetically different but semantically identical call.
	KeyArgs []string

	// IgnoreArgs names arguments excluded from the key. Applied after KeyArgs.
	// Use it for arguments that do not change the result: a request ID, a
	// verbosity flag, a trace token.
	IgnoreArgs []string

	// VaryOnContext names ToolContext knobs that change the result and must
	// therefore be in the key. This is the field most easily forgotten, and
	// forgetting it is how a tool configured with root=/a serves results
	// cached under root=/b.
	VaryOnContext []string

	// CacheErrors stores failed results too. Off by default: a transient
	// failure cached for an hour turns one outage into an hour of outage. Turn
	// it on only for deterministic failures, such as a validation error.
	CacheErrors bool

	// ServeStaleOnError returns an expired entry when a refresh fails, instead
	// of propagating the error. Trades correctness for availability, which is
	// usually right for enrichment and wrong for anything authoritative.
	ServeStaleOnError bool
	// MaxStale bounds how far past TTL a stale entry may still be served. Zero
	// with ServeStaleOnError set means unbounded, which Validate rejects: an
	// answer from last month is not a degraded answer, it is a wrong one.
	MaxStale time.Duration

	// MaxEntries caps entries for this tool. Zero defers to the backing store's
	// own limit.
	MaxEntries int
}

// Validate reports whether the policy is coherent. It catches the
// configurations that look enabled but behave badly rather than failing.
func (p CachePolicy) Validate() error {
	if !p.Enabled {
		return nil
	}
	if p.TTL <= 0 {
		return fmt.Errorf("cache policy: Enabled requires a positive TTL")
	}
	if p.ServeStaleOnError && p.MaxStale <= 0 {
		return fmt.Errorf("cache policy: ServeStaleOnError requires a positive MaxStale")
	}
	if p.MaxStale < 0 {
		return fmt.Errorf("cache policy: MaxStale must not be negative")
	}
	if len(p.KeyArgs) > 0 && len(p.IgnoreArgs) > 0 {
		for _, k := range p.KeyArgs {
			for _, i := range p.IgnoreArgs {
				if k == i {
					return fmt.Errorf("cache policy: %q is in both KeyArgs and IgnoreArgs", k)
				}
			}
		}
	}
	switch p.Scope {
	case "", CacheScopeGlobal, CacheScopeSession, CacheScopeAgent, CacheScopeUser:
	default:
		return fmt.Errorf("cache policy: unknown scope %q", p.Scope)
	}
	return nil
}

// EffectiveScope resolves the scope, defaulting to session.
func (p CachePolicy) EffectiveScope() CacheScope {
	if p.Scope == "" {
		return CacheScopeSession
	}
	return p.Scope
}

// KeyArguments returns the arguments that belong in the key, after applying
// KeyArgs and IgnoreArgs. The result is sorted so map iteration order cannot
// produce two different keys for one call.
func (p CachePolicy) KeyArguments(args map[string]any) []string {
	ignored := make(map[string]bool, len(p.IgnoreArgs))
	for _, k := range p.IgnoreArgs {
		ignored[k] = true
	}

	var names []string
	if len(p.KeyArgs) > 0 {
		for _, k := range p.KeyArgs {
			if !ignored[k] {
				names = append(names, k)
			}
		}
	} else {
		for k := range args {
			if !ignored[k] {
				names = append(names, k)
			}
		}
	}
	sort.Strings(names)
	return names
}

// Describe renders the policy for logs and CLI listings, so an operator can see
// what a tool's caching actually does without reading its source.
func (p CachePolicy) Describe() string {
	if !p.Enabled {
		return "uncached"
	}
	parts := []string{
		"scope=" + string(p.EffectiveScope()),
		"ttl=" + p.TTL.String(),
	}
	if len(p.KeyArgs) > 0 {
		parts = append(parts, "key="+strings.Join(p.KeyArgs, "+"))
	}
	if len(p.IgnoreArgs) > 0 {
		parts = append(parts, "ignore="+strings.Join(p.IgnoreArgs, "+"))
	}
	if len(p.VaryOnContext) > 0 {
		parts = append(parts, "vary="+strings.Join(p.VaryOnContext, "+"))
	}
	if p.CacheErrors {
		parts = append(parts, "cache-errors")
	}
	if p.ServeStaleOnError {
		parts = append(parts, "stale-ok<="+p.MaxStale.String())
	}
	return strings.Join(parts, " ")
}

// Cacheable is an OPTIONAL interface a tool implements to declare its own cache
// policy. The tool author knows whether its results are stable and for how
// long; the deployment only knows whether it wants a cache at all.
type Cacheable interface {
	Tool
	CachePolicy() CachePolicy
}

// PolicyFor returns a tool's declared policy, or the zero (uncached) policy.
func PolicyFor(tool Tool) CachePolicy {
	if c, ok := tool.(Cacheable); ok {
		return c.CachePolicy()
	}
	return CachePolicy{}
}
