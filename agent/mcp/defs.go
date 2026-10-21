package mcp

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// ErrUnsupportedSchema reports a tool input schema whose references
// InlineDefs cannot resolve: a $ref that is not a local "#/$defs/<name>"
// reference, names no definition, is recursive, or expands past
// MaxInlinedRefs references. The error names the location in the schema and
// the reason.
var ErrUnsupportedSchema = errors.New("mcp: unsupported input schema")

// MaxInlinedRefs bounds how many references InlineDefs expands in one
// schema, so a small schema whose definitions reference each other many
// times over cannot grow without limit.
const MaxInlinedRefs = 1000

// defsPrefix is the only reference form InlineDefs resolves.
const defsPrefix = "#/$defs/"

// InlineStats describes what InlineDefs did, for a debug log.
type InlineStats struct {
	// Defs is how many definitions the schema carried in $defs.
	Defs int
	// Inlined is how many references were replaced by their definition.
	Inlined int
}

// InlineDefs returns a copy of schema with every {"$ref": "#/$defs/<name>"}
// replaced by a copy of the definition it names, and $defs removed. Tool
// schemas built by pydantic and similar generators factor shared shapes
// into $defs, which a model provider's tool format cannot express.
//
// Keywords written beside a $ref (a description, a default) are kept and win
// over the definition's. A definition used twice is copied twice, never
// shared. A reference is refused, with an error wrapping ErrUnsupportedSchema
// that names the location and the reason, when it is not a string, is not a
// local #/$defs reference (including the older #/definitions form), names no
// definition (or points inside one), or reaches itself through the
// definitions it expands, since a recursive type has no finite inline form.
// Expanding more than MaxInlinedRefs references is refused too. schema itself
// is not modified.
func InlineDefs(schema map[string]any) (map[string]any, InlineStats, error) {
	var stats InlineStats
	if schema == nil {
		return nil, stats, nil
	}
	doc := deepCopy(schema).(map[string]any)
	var defs map[string]any
	if v, ok := doc["$defs"]; ok {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, stats, fmt.Errorf("%w: schema at $.$defs is not an object", ErrUnsupportedSchema)
		}
		defs = m
		delete(doc, "$defs")
	}
	stats.Defs = len(defs)
	in := &inliner{defs: defs, stats: &stats}
	out, err := in.walk(doc, "$", nil)
	if err != nil {
		return nil, stats, err
	}
	return out.(map[string]any), stats, nil
}

type inliner struct {
	defs  map[string]any
	stats *InlineStats
}

// walk returns v with its references inlined, rewriting v's maps and slices
// in place. chain lists the definitions being expanded around v, outermost
// first, to catch recursion.
func (in *inliner) walk(v any, path string, chain []string) (any, error) {
	switch n := v.(type) {
	case []any:
		for i, e := range n {
			out, err := in.walk(e, path+"["+strconv.Itoa(i)+"]", chain)
			if err != nil {
				return nil, err
			}
			n[i] = out
		}
		return n, nil
	case map[string]any:
		if ref, ok := n["$ref"]; ok {
			return in.expand(n, ref, path, chain)
		}
		for k, e := range n {
			out, err := in.walk(e, childPath(path, k), chain)
			if err != nil {
				return nil, err
			}
			n[k] = out
		}
		return n, nil
	default:
		return v, nil
	}
}

func (in *inliner) expand(node map[string]any, ref any, path string, chain []string) (any, error) {
	s, ok := ref.(string)
	if !ok {
		return nil, fmt.Errorf("%w: schema at %s has a $ref that is not a string, which is not supported", ErrUnsupportedSchema, path)
	}
	if !strings.HasPrefix(s, defsPrefix) {
		return nil, fmt.Errorf("%w: schema at %s uses $ref %q, which is not supported: only local %s<name> references are resolved",
			ErrUnsupportedSchema, path, s, defsPrefix)
	}
	rest := strings.TrimPrefix(s, defsPrefix)
	name := unescapePointer(rest)
	def, ok := in.defs[name]
	if !ok || strings.Contains(rest, "/") {
		return nil, fmt.Errorf("%w: schema at %s uses $ref %q, which is not supported: it names no definition in $defs (have %s)",
			ErrUnsupportedSchema, path, s, defNames(in.defs))
	}
	for i, c := range chain {
		if c == name {
			cycle := append(append([]string{}, chain[i:]...), name)
			return nil, fmt.Errorf("%w: schema at %s uses $ref %q, which is not supported: the definition is recursive (%s)",
				ErrUnsupportedSchema, path, s, strings.Join(cycle, " -> "))
		}
	}
	body, ok := def.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: schema at $.$defs.%s is not an object", ErrUnsupportedSchema, name)
	}
	in.stats.Inlined++
	if in.stats.Inlined > MaxInlinedRefs {
		return nil, fmt.Errorf("%w: schema at %s: inlining $defs expands more than %d references", ErrUnsupportedSchema, path, MaxInlinedRefs)
	}
	expanded, err := in.walk(deepCopy(body), path, append(chain, name))
	if err != nil {
		return nil, err
	}
	out := expanded.(map[string]any)
	for k, v := range node {
		if k == "$ref" {
			continue
		}
		v, err := in.walk(v, childPath(path, k), chain)
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

// childPath names a schema location: a property by its name, array items
// as [], any other keyword by itself.
func childPath(path, key string) string {
	switch key {
	case "properties":
		return path
	case "items":
		return path + "[]"
	}
	return path + "." + key
}

// unescapePointer decodes a JSON pointer token (RFC 6901).
func unescapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~1", "/"), "~0", "~")
}

func defNames(defs map[string]any) string {
	if len(defs) == 0 {
		return "none"
	}
	names := make([]string, 0, len(defs))
	for k := range defs {
		names = append(names, k)
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

func deepCopy(v any) any {
	switch n := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(n))
		for k, e := range n {
			out[k] = deepCopy(e)
		}
		return out
	case []any:
		out := make([]any, len(n))
		for i, e := range n {
			out[i] = deepCopy(e)
		}
		return out
	default:
		return v
	}
}
