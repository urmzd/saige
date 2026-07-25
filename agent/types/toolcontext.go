package types

import (
	"context"
	"fmt"
	"sort"
)

// A tool has two kinds of input that are not its arguments, and conflating them
// is what makes tools hard to reuse:
//
//   - Context: knobs a deployment or a caller sets to change behaviour. A
//     search tool's result limit, a file tool's root directory, a summariser's
//     target length. These are data, they change between runs and sometimes
//     between calls, and the model must never see or set them.
//   - Dependencies: services supplied from upstream. A database pool, an HTTP
//     client, an embedder, a logger. These are wired once at construction and
//     do not vary per call.
//
// Both used to be closed over at construction, which meant changing a limit
// required rebuilding the tool and its dependency graph. Splitting them lets
// context vary dynamically over a tool whose dependencies are fixed.

// ToolContext is an immutable set of named knobs. Immutable because a tool may
// run concurrently with itself: a map a caller mutated between fan-out
// goroutines would give two parallel calls different configuration with no
// ordering guarantee about which.
type ToolContext struct {
	values map[string]any
}

// NewToolContext builds a context from a map. The map is copied, so later
// mutation by the caller cannot reach the tool.
func NewToolContext(values map[string]any) ToolContext {
	out := ToolContext{values: make(map[string]any, len(values))}
	for k, v := range values {
		out.values[k] = v
	}
	return out
}

// With returns a copy with one key set. Chaining builds a per-call override on
// top of a deployment default without disturbing it.
func (c ToolContext) With(key string, value any) ToolContext {
	out := ToolContext{values: make(map[string]any, len(c.values)+1)}
	for k, v := range c.values {
		out.values[k] = v
	}
	out.values[key] = value
	return out
}

// Merge returns a copy with every key from other applied on top.
func (c ToolContext) Merge(other ToolContext) ToolContext {
	out := c
	for k, v := range other.values {
		out = out.With(k, v)
	}
	return out
}

// Value returns a raw knob.
func (c ToolContext) Value(key string) (any, bool) {
	v, ok := c.values[key]
	return v, ok
}

// String, Int, Float and Bool are typed accessors that fall back to def when
// the key is absent or the wrong type. They never error: a misconfigured knob
// should degrade to the default, not fail a tool call the model is waiting on.
func (c ToolContext) String(key, def string) string {
	if v, ok := c.values[key].(string); ok {
		return v
	}
	return def
}

func (c ToolContext) Int(key string, def int) int {
	switch v := c.values[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64: // JSON-decoded config arrives as float64
		return int(v)
	}
	return def
}

func (c ToolContext) Float(key string, def float64) float64 {
	switch v := c.values[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	}
	return def
}

func (c ToolContext) Bool(key string, def bool) bool {
	if v, ok := c.values[key].(bool); ok {
		return v
	}
	return def
}

// Keys returns every set knob, sorted. Used for cache keys and for logging what
// a call actually ran with.
func (c ToolContext) Keys() []string {
	out := make([]string, 0, len(c.values))
	for k := range c.values {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Len returns how many knobs are set.
func (c ToolContext) Len() int { return len(c.values) }

// ── Propagation through context.Context ─────────────────────────────

type toolContextKey struct{}

// WithToolContext attaches a ToolContext to a context.Context, so a tool can
// read per-call knobs without every Tool signature growing a parameter.
//
// The agent loop attaches the agent's configured ToolContext before each call,
// so a tool that reads knobs works whether it was configured at construction or
// handed them per invocation.
func WithToolContext(ctx context.Context, tc ToolContext) context.Context {
	return context.WithValue(ctx, toolContextKey{}, tc)
}

// ToolContextFrom returns the attached ToolContext, or an empty one. Never nil,
// so a tool can call accessors unconditionally.
func ToolContextFrom(ctx context.Context) ToolContext {
	if tc, ok := ctx.Value(toolContextKey{}).(ToolContext); ok {
		return tc
	}
	return ToolContext{}
}

// ── Dependencies ────────────────────────────────────────────────────

// Deps is a named set of services supplied from upstream. It is deliberately
// untyped: the alternative is a struct in this package listing every service
// any tool might want, which makes types depend on postgres, HTTP clients and
// embedders, and forces a change here for every new tool.
//
// Tools declare what they need by key via Configurable.Requires, and wiring
// fails loudly at construction when a key is missing rather than nil-panicking
// on the first call.
type Deps struct {
	values map[string]any
}

// NewDeps builds a dependency set.
func NewDeps(values map[string]any) Deps {
	out := Deps{values: make(map[string]any, len(values))}
	for k, v := range values {
		out.values[k] = v
	}
	return out
}

// With returns a copy with one dependency set.
func (d Deps) With(key string, value any) Deps {
	out := Deps{values: make(map[string]any, len(d.values)+1)}
	for k, v := range d.values {
		out.values[k] = v
	}
	out.values[key] = value
	return out
}

// Get returns a dependency.
func (d Deps) Get(key string) (any, bool) {
	v, ok := d.values[key]
	return v, ok
}

// Has reports whether a key is present.
func (d Deps) Has(key string) bool {
	_, ok := d.values[key]
	return ok
}

// Keys returns every supplied key, sorted.
func (d Deps) Keys() []string {
	out := make([]string, 0, len(d.values))
	for k := range d.values {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Missing returns the required keys this set does not supply.
func (d Deps) Missing(required ...string) []string {
	var out []string
	for _, k := range required {
		if !d.Has(k) {
			out = append(out, k)
		}
	}
	return out
}

// Dep fetches a typed dependency. The type parameter is the whole point: a
// dependency present under the right key but the wrong type is a wiring bug
// that must surface at construction, not as a panic mid-run.
func Dep[T any](d Deps, key string) (T, error) {
	var zero T
	v, ok := d.values[key]
	if !ok {
		return zero, fmt.Errorf("deps: missing dependency %q", key)
	}
	t, ok := v.(T)
	if !ok {
		return zero, fmt.Errorf("deps: dependency %q is %T, want %T", key, v, zero)
	}
	return t, nil
}

// ── Configurable tools ──────────────────────────────────────────────

// Configurable is an OPTIONAL interface for a tool whose behaviour is bound
// from context and dependencies rather than fixed at construction.
//
// Configure returns a *new* tool rather than mutating the receiver, so one
// registered prototype can produce differently-configured instances for
// different agents, sub-agents, or tenants without any of them sharing state.
type Configurable interface {
	Tool
	// ContextSchema declares the knobs this tool reads. It is the same shape as
	// a parameter schema so a deployment can validate a config file, render a
	// settings form, or document the tool from one declaration.
	ContextSchema() ParameterSchema
	// Requires names the dependency keys the tool needs.
	Requires() []string
	// Configure binds the tool to a context and dependency set.
	Configure(tc ToolContext, deps Deps) (Tool, error)
}

// Configure binds a tool if it is Configurable, and returns it unchanged
// otherwise, so callers can run every tool through the same wiring step.
// A Configurable whose dependencies are not satisfied is an error here rather
// than a nil dereference on its first call.
func Configure(tool Tool, tc ToolContext, deps Deps) (Tool, error) {
	c, ok := tool.(Configurable)
	if !ok {
		return tool, nil
	}
	if missing := deps.Missing(c.Requires()...); len(missing) > 0 {
		return nil, fmt.Errorf("tool %q is missing dependencies %v", tool.Definition().Name, missing)
	}
	return c.Configure(tc, deps)
}

// ConfigureAll binds every tool in a registry and returns a new registry of the
// bound instances. The source registry is left alone so a prototype set can be
// configured many times.
func ConfigureAll(src *ToolRegistry, tc ToolContext, deps Deps) (*ToolRegistry, error) {
	out := NewToolRegistry()
	for _, t := range src.All() {
		bound, err := Configure(t, tc, deps)
		if err != nil {
			return nil, err
		}
		out.Register(bound)
	}
	return out, nil
}
