package event

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sync"
)

// Entry registers one type.
type Entry struct {
	// Type is the type name, such as "run.completed.v1".
	Type string
	// New returns a pointer to a zero body to decode into.
	New func() Body
	// Render turns an event of this type into the text a model sees. It must
	// be byte stable: the same event renders the same bytes on every replay.
	Render func(Envelope) (string, error)
	// Sensitive marks a body that carries human-authored free text or private
	// host data. It is rendered to the model, and egress adapters and logs
	// omit it unless the subscriber opts in explicitly.
	Sensitive bool
	// OptIn keeps the type out of a subscription that does not name its
	// family explicitly (see Selects).
	OptIn bool
}

type catalog struct {
	mu      sync.RWMutex
	entries map[string]Entry
}

var std = &catalog{}

// Register adds an entry to the catalog. It panics on a duplicate type, a
// name outside the grammar, a nil New or Render, or a New whose body names a
// different type: a catalog that cannot decode or render a type is a
// programming error, not a runtime condition.
func Register(e Entry) { std.register(e) }

// Lookup returns the entry for a type.
func Lookup(t string) (Entry, bool) { return std.lookup(t) }

// Types returns every registered type, sorted.
func Types() []string { return std.types() }

// Decode decodes data as the body of type t. A type the catalog does not hold
// decodes to Unknown, never to an error. Data of a registered type that does
// not decode is an error. Empty or null data decodes to the zero body.
func Decode(t string, data json.RawMessage) (Body, error) { return std.decode(t, data) }

func (c *catalog) register(e Entry) {
	switch {
	case !ValidType(e.Type):
		panic(fmt.Sprintf("event: register %q: not a valid type", e.Type))
	case e.New == nil:
		panic(fmt.Sprintf("event: register %q: nil New", e.Type))
	case e.Render == nil:
		panic(fmt.Sprintf("event: register %q: nil Render", e.Type))
	}
	b := e.New()
	if b == nil || reflect.ValueOf(b).Kind() != reflect.Pointer || reflect.ValueOf(b).IsNil() {
		panic(fmt.Sprintf("event: register %q: New must return a non-nil pointer", e.Type))
	}
	if got := b.Type(); got != e.Type {
		panic(fmt.Sprintf("event: register %q: New returns a body of type %q", e.Type, got))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, dup := c.entries[e.Type]; dup {
		panic(fmt.Sprintf("event: register %q: duplicate type", e.Type))
	}
	if c.entries == nil {
		c.entries = make(map[string]Entry)
	}
	c.entries[e.Type] = e
}

func (c *catalog) lookup(t string) (Entry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[t]
	return e, ok
}

func (c *catalog) types() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(c.entries))
	for t := range c.entries {
		out = append(out, t)
	}
	slices.Sort(out)
	return out
}

func (c *catalog) decode(t string, data json.RawMessage) (Body, error) {
	e, ok := c.lookup(t)
	if !ok {
		return Unknown{TypeName: t, Raw: slices.Clone(data)}, nil
	}
	ptr := e.New()
	if len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, ptr); err != nil {
			return nil, fmt.Errorf("event: decode %s: %w", t, err)
		}
	}
	// Bodies are values: return what the pointer points at.
	body, ok := reflect.ValueOf(ptr).Elem().Interface().(Body)
	if !ok {
		return nil, fmt.Errorf("event: decode %s: %T is not a body by value", t, ptr)
	}
	return body, nil
}
