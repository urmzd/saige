package run

import (
	"context"
	"fmt"
	"sync"
)

// WorkflowFn is the type-erased registration seam between the generic SDK
// and the worker: input bytes in, output bytes out, with the replay frame
// already in ctx. The root package's Activity wrapper compiles down to one
// of these; pkg/worker never sees a generic type.
type WorkflowFn func(ctx context.Context, input []byte) ([]byte, error)

// Registry maps workflow names to their erased functions. Only entry
// activities (those handed to Start) are looked up here — child calls are
// invoked directly by the re-executing parent.
type Registry struct {
	mu  sync.RWMutex
	fns map[string]WorkflowFn
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{fns: make(map[string]WorkflowFn)}
}

// Register adds fn under name. Duplicate names panic: registration happens
// at package-var init, and two activities sharing a name is a deterministic
// programming error better caught at process start than at 2am dispatch.
func (r *Registry) Register(name string, fn WorkflowFn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.fns[name]; ok {
		panic(fmt.Sprintf("durable: activity %q registered twice", name))
	}
	r.fns[name] = fn
}

// Lookup returns the function registered under name.
func (r *Registry) Lookup(name string) (WorkflowFn, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	fn, ok := r.fns[name]
	return fn, ok
}

// DefaultRegistry receives every Activity declared without an explicit
// registry. Workers consume it by default; tests isolate with their own.
var DefaultRegistry = NewRegistry()
