package native

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	saigetypes "github.com/urmzd/saige/agent/types"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connector"
	"github.com/urmzd/saige/flow/event"
)

// Tools resolves the names in Spec.Tools to executable tools.
//
// A spec grants tools by NAME, and names are resolved per deployment: the same
// agent definition runs in a process with a Jira connector and in one without,
// and which of those is an error is a deployment decision, not a definition
// one. Lookup therefore returns an error rather than a bool, so the message can
// say what was missing. Any error makes the tool unavailable to the run (see
// Runner.Unavailable); the error is the reason the operator is shown.
type Tools interface {
	Lookup(name string) (saigetypes.Tool, error)
}

// ToolsFunc adapts a function into Tools.
type ToolsFunc func(name string) (saigetypes.Tool, error)

// Lookup implements Tools.
func (f ToolsFunc) Lookup(name string) (saigetypes.Tool, error) { return f(name) }

// ErrNoSuchTool reports a spec naming a tool this deployment has not loaded.
var ErrNoSuchTool = fmt.Errorf("%w: no such tool", ErrInvalid)

// ConnectorTools resolves tool names against the actions every registered
// connector contributes, and binds each one to a target.
//
// The target is where the action lands: the surface the tag came from, or a
// route's destination. It is bound here rather than passed by the model,
// because an action's target is an authorization fact (the tag arrived on that
// surface), and a model that could name its own target could post anywhere the
// credential reaches.
//
// after is called for every action that runs, successfully or not. pkg/runtime
// passes a function that publishes run.action.taken.v1, which is what puts
// "the work got done" on the bus next to the conversation.
func ConnectorTools(reg *connector.Registry, target address.Address, after func(event.Action) error) Tools {
	return ConnectorToolsForRun(reg, target, "", after)
}

// ConnectorToolsForRun also binds the durable run identity for actions whose
// authorization must not be replayed from another run.
func ConnectorToolsForRun(reg *connector.Registry, target address.Address, runID string, after func(event.Action) error) Tools {
	return ToolsFunc(func(name string) (saigetypes.Tool, error) {
		if reg == nil {
			return nil, fmt.Errorf("%w: %q (no connector registry configured)", ErrNoSuchTool, name)
		}
		for _, a := range reg.Actions() {
			if a.Name == name {
				invoke := a.Invoke
				if invoke != nil {
					a.Invoke = func(ctx context.Context, target address.Address, args json.RawMessage) (connector.Result, error) {
						return invoke(connector.WithRunID(ctx, runID), target, args)
					}
				}
				return ActionTool(a, target, after)
			}
		}
		return nil, fmt.Errorf("%w: %q is not contributed by any registered connector %v", ErrNoSuchTool, name, reg.Names())
	})
}

// StaticTools resolves names against a fixed set, for deployments (and tests)
// whose tools are not connector actions.
func StaticTools(tools ...saigetypes.Tool) Tools {
	byName := make(map[string]saigetypes.Tool, len(tools))
	for _, t := range tools {
		byName[t.Definition().Name] = t
	}
	return ToolsFunc(func(name string) (saigetypes.Tool, error) {
		t, ok := byName[name]
		if !ok {
			names := make([]string, 0, len(byName))
			for n := range byName {
				names = append(names, n)
			}
			sort.Strings(names)
			return nil, fmt.Errorf("%w: %q, have %v", ErrNoSuchTool, name, names)
		}
		return t, nil
	})
}

// ActionTool wraps a connector's native verb as a tool the model can call.
//
// connector.Action describes its arguments as JSON Schema (what a connector
// author writes and what every other tool protocol speaks), and saige's
// ParameterSchema decodes that JSON directly. See parameterSchema for the
// checks applied on top.
func ActionTool(a connector.Action, target address.Address, after func(event.Action) error) (saigetypes.Tool, error) {
	schema, err := parameterSchema(a.Schema)
	if err != nil {
		return nil, fmt.Errorf("native: action %q: %w", a.Name, err)
	}
	if a.Invoke == nil {
		return nil, fmt.Errorf("%w: action %q has no Invoke", ErrInvalid, a.Name)
	}
	return &actionTool{action: a, target: target, after: after, def: saigetypes.ToolDef{
		Name:        a.Name,
		Description: a.Description,
		Parameters:  schema,
	}}, nil
}

type actionTool struct {
	action connector.Action
	target address.Address
	after  func(event.Action) error
	def    saigetypes.ToolDef
}

func (t *actionTool) Definition() saigetypes.ToolDef { return t.def }

// Execute invokes the connector action and reports it.
//
// A failed action returns its error to the model as text rather than as a Go
// error, so the agent can react (apologize, try a different verb, ask a human)
// instead of the whole turn dying on one refused API call. The report goes out
// either way: an attempted action that failed is exactly as auditable as one
// that worked.
func (t *actionTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("native: action %q: marshal arguments: %w", t.action.Name, err)
	}
	res, invokeErr := t.action.Invoke(ctx, t.target, raw)

	rec := event.Action{
		Name:    t.action.Name,
		Target:  t.target.String(),
		Summary: res.Summary,
		Address: res.Address.String(),
		Data:    res.Data,
	}
	if invokeErr != nil {
		rec.Error = invokeErr.Error()
	}
	if t.after != nil {
		if err := t.after(rec); err != nil {
			return "", fmt.Errorf("native: action %q: report: %w", t.action.Name, err)
		}
	}
	if invokeErr != nil {
		return "action failed: " + invokeErr.Error(), nil
	}
	if res.Summary == "" {
		return "done", nil
	}
	return res.Summary, nil
}

// schemaRefs captures the JSON Schema keyword ParameterSchema does not model
// and that would change a schema's meaning if dropped.
type schemaRefs struct {
	Defs map[string]json.RawMessage `json:"$defs,omitempty"`
}

// parameterSchema decodes a JSON Schema object into saige's form. An empty
// schema is legal and means "no arguments"; anything that is not an object is
// rejected, since a tool call's arguments are always a named set.
func parameterSchema(raw json.RawMessage) (saigetypes.ParameterSchema, error) {
	if len(raw) == 0 {
		return saigetypes.ParameterSchema{Type: "object"}, nil
	}
	var s saigetypes.ParameterSchema
	if err := json.Unmarshal(raw, &s); err != nil {
		return saigetypes.ParameterSchema{}, fmt.Errorf("schema is not JSON Schema: %w", err)
	}
	var refs schemaRefs
	if err := json.Unmarshal(raw, &refs); err != nil {
		return saigetypes.ParameterSchema{}, fmt.Errorf("schema is not JSON Schema: %w", err)
	}
	if s.Type == "" {
		s.Type = "object"
	}
	if s.Type != "object" {
		return saigetypes.ParameterSchema{}, fmt.Errorf("%w: schema type %q, want object", ErrInvalid, s.Type)
	}
	if len(refs.Defs) > 0 {
		// $ref resolution is not implemented, and a tool whose arguments
		// depend on unresolved refs would be shown to the model with holes
		// in it. Refusing names the limitation at build time.
		return saigetypes.ParameterSchema{}, fmt.Errorf("%w: schema uses $defs/$ref, which is not supported", ErrInvalid)
	}
	return s, nil
}
