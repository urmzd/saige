package eval

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// ScorerSpec names a registered scorer kind and its parameters, so a suite's
// scorers can be declared in configuration instead of code:
//
//	{"kind": "regex_count", "name": "citations", "params": {"pattern": "\\[\\d+\\]", "op": ">=", "count": 2}}
//
// Name, when set, renames the metric as with [Named].
type ScorerSpec struct {
	Kind   string          `json:"kind"`
	Name   string          `json:"name,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

// ScorerFactory builds a scorer from a spec's parameters. params is nil when
// the spec has none.
type ScorerFactory func(params json.RawMessage) (Scorer, error)

// ScorerKind describes one registered kind.
type ScorerKind struct {
	Kind        string `json:"kind"`
	Description string `json:"description"`
}

// ErrUnknownScorer is returned by [Registry.Build] for an unregistered kind.
var ErrUnknownScorer = errors.New("unknown scorer kind")

// Registry maps scorer kinds to factories. It is safe for concurrent use.
type Registry struct {
	mu        sync.RWMutex
	factories map[string]registeredScorer
}

type registeredScorer struct {
	description string
	factory     ScorerFactory
}

// NewRegistry returns an empty registry. Use [NewBuiltinRegistry] for one
// that already holds this package's scorers.
func NewRegistry() *Registry {
	return &Registry{factories: map[string]registeredScorer{}}
}

// Register adds a kind. Registering a kind twice is an error, so two
// packages cannot silently replace each other's scorers.
func (r *Registry) Register(kind, description string, factory ScorerFactory) error {
	if kind == "" || factory == nil {
		return errors.New("register scorer: kind and factory are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.factories[kind]; exists {
		return fmt.Errorf("register scorer: kind %q is already registered", kind)
	}
	r.factories[kind] = registeredScorer{description: description, factory: factory}
	return nil
}

// Build constructs the scorer a spec describes.
func (r *Registry) Build(spec ScorerSpec) (Scorer, error) {
	r.mu.RLock()
	reg, ok := r.factories[spec.Kind]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownScorer, spec.Kind)
	}
	params := spec.Params
	if len(bytes.TrimSpace(params)) == 0 || string(bytes.TrimSpace(params)) == typeNull {
		params = nil
	}
	s, err := reg.factory(params)
	if err != nil {
		return nil, fmt.Errorf("scorer %q: %w", spec.Kind, err)
	}
	if spec.Name != "" {
		s = Named(s, spec.Name)
	}
	return s, nil
}

// BuildAll constructs every spec, stopping at the first error.
func (r *Registry) BuildAll(specs []ScorerSpec) ([]Scorer, error) {
	out := make([]Scorer, 0, len(specs))
	for i, spec := range specs {
		s, err := r.Build(spec)
		if err != nil {
			return nil, fmt.Errorf("scorer %d: %w", i, err)
		}
		out = append(out, s)
	}
	return out, nil
}

// Kinds lists the registered kinds in name order.
func (r *Registry) Kinds() []ScorerKind {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ScorerKind, 0, len(r.factories))
	for kind, reg := range r.factories {
		out = append(out, ScorerKind{Kind: kind, Description: reg.description})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// DefaultRegistry holds this package's built-in scorers. Subsystem packages
// such as agent/eval add their own kinds to it when imported.
var DefaultRegistry = NewBuiltinRegistry()

// RegisterScorer adds a kind to [DefaultRegistry].
func RegisterScorer(kind, description string, factory ScorerFactory) error {
	return DefaultRegistry.Register(kind, description, factory)
}

// BuildScorer constructs a scorer from [DefaultRegistry].
func BuildScorer(spec ScorerSpec) (Scorer, error) {
	return DefaultRegistry.Build(spec)
}

// DecodeParams decodes a spec's parameters into v, rejecting unknown fields
// so a misspelled parameter fails instead of falling back to a default. Nil
// params leave v unchanged.
func DecodeParams(params json.RawMessage, v any) error {
	if params == nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(params))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("params: %w", err)
	}
	return nil
}

// NoParams adapts a scorer constructor that takes no parameters into a
// [ScorerFactory] that rejects any.
func NoParams(build func() Scorer) ScorerFactory {
	return func(params json.RawMessage) (Scorer, error) {
		if params != nil && string(bytes.TrimSpace(params)) != "{}" {
			return nil, errors.New("takes no params")
		}
		return build(), nil
	}
}

// NewBuiltinRegistry returns a new registry holding this package's built-in
// scorers. LLM judges are not registered because they need a [Generator];
// construct them in code.
func NewBuiltinRegistry() *Registry {
	r := NewRegistry()
	must := func(kind, desc string, f ScorerFactory) {
		if err := r.Register(kind, desc, f); err != nil {
			panic(err)
		}
	}

	must("sequence_similarity", "character LCS ratio between output and ground truth", NoParams(SequenceSimilarityScorer))
	must("token_f1", "word-token F1 between output and ground truth", NoParams(TokenF1Scorer))
	must("rouge_l", "ROUGE-L F1 between output and ground truth", NoParams(RougeLScorer))
	must("exact_match", "output text equals ground truth text after trimming whitespace", NoParams(ExactMatchScorer))
	must("cost_usd", "reports the recorded cost in USD", NoParams(CostScorer))
	must("total_tokens", "reports input plus output tokens", NoParams(TotalTokensScorer))

	must("contains", `output contains every substring; params: {"substrings": [...]}`,
		func(params json.RawMessage) (Scorer, error) {
			var p struct {
				Substrings []string `json:"substrings"`
			}
			if err := DecodeParams(params, &p); err != nil {
				return nil, err
			}
			if len(p.Substrings) == 0 {
				return nil, errors.New("params: substrings is required")
			}
			return ContainsScorer(p.Substrings...), nil
		})
	must("regex_count", `count of pattern matches in the output, compared with op; params: {"pattern", "op": ">=|<=|==", "count"}`,
		func(params json.RawMessage) (Scorer, error) {
			p := struct {
				Pattern string `json:"pattern"`
				Op      Op     `json:"op"`
				Count   int    `json:"count"`
			}{Op: GTE, Count: 1}
			if err := DecodeParams(params, &p); err != nil {
				return nil, err
			}
			if p.Pattern == "" {
				return nil, errors.New("params: pattern is required")
			}
			return RegexCountScorer(p.Pattern, p.Op, p.Count)
		})
	must("json_schema", `output validates against a JSON Schema; params: {"schema": {...}}`,
		func(params json.RawMessage) (Scorer, error) {
			var p struct {
				Schema json.RawMessage `json:"schema"`
			}
			if err := DecodeParams(params, &p); err != nil {
				return nil, err
			}
			if len(p.Schema) == 0 {
				return nil, errors.New("params: schema is required")
			}
			return JSONSchemaScorer(p.Schema)
		})
	must("json_field", `output field at a dot path equals a value or the ground truth field; params: {"path", "equals"} or {"path", "from_ground_truth": true}`,
		func(params json.RawMessage) (Scorer, error) {
			var p struct {
				Path            string          `json:"path"`
				Equals          json.RawMessage `json:"equals"`
				FromGroundTruth bool            `json:"from_ground_truth"`
			}
			if err := DecodeParams(params, &p); err != nil {
				return nil, err
			}
			switch {
			case p.FromGroundTruth && p.Equals != nil:
				return nil, errors.New("params: equals and from_ground_truth are exclusive")
			case p.FromGroundTruth:
				return JSONFieldGroundTruthScorer(p.Path), nil
			case p.Equals == nil:
				return nil, errors.New("params: equals or from_ground_truth is required")
			}
			var want any
			if err := json.Unmarshal(p.Equals, &want); err != nil {
				return nil, fmt.Errorf("params: equals: %w", err)
			}
			return JSONFieldScorer(p.Path, want), nil
		})
	must("token_budget", `input plus output tokens at most max_tokens; params: {"max_tokens"}`,
		func(params json.RawMessage) (Scorer, error) {
			var p struct {
				MaxTokens *int `json:"max_tokens"`
			}
			if err := DecodeParams(params, &p); err != nil {
				return nil, err
			}
			if p.MaxTokens == nil {
				return nil, errors.New("params: max_tokens is required")
			}
			return TokenBudgetScorer(*p.MaxTokens), nil
		})
	must("cost_budget", `recorded cost at most max_usd; params: {"max_usd"}`,
		func(params json.RawMessage) (Scorer, error) {
			var p struct {
				MaxUSD *float64 `json:"max_usd"`
			}
			if err := DecodeParams(params, &p); err != nil {
				return nil, err
			}
			if p.MaxUSD == nil {
				return nil, errors.New("params: max_usd is required")
			}
			return CostBudgetScorer(*p.MaxUSD), nil
		})
	must("responds_within", `total time at most max_ms; params: {"max_ms"}`,
		func(params json.RawMessage) (Scorer, error) {
			var p struct {
				MaxMs *int64 `json:"max_ms"`
			}
			if err := DecodeParams(params, &p); err != nil {
				return nil, err
			}
			if p.MaxMs == nil {
				return nil, errors.New("params: max_ms is required")
			}
			return RespondsWithinScorer(time.Duration(*p.MaxMs) * time.Millisecond), nil
		})
	return r
}
