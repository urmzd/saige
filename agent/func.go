package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/agent/workspace"
)

// RunContext is what a Func tool receives besides its typed input: the call's
// context, the host's typed dependencies, and the facts the agent loop knows
// about the call. It embeds the context.Context, so it can be passed wherever
// a context is expected.
//
// It replaces untyped types.ToolContext lookups for new code. Knobs set on
// the agent with WithToolContext remain readable through Knobs.
type RunContext[D any] struct {
	context.Context
	// Deps are the host's dependencies, from ContextWithDeps or WithDeps.
	Deps D
	// Call identifies the tool call: its ID, the tool name, the agent that
	// owns the turn, the root run ID and the branch.
	Call types.ToolCallInfo
	// Workspace is the run's scratch store, nil when none is configured.
	Workspace workspace.Workspace
	// IdempotencyKey is stable across replays of this call in a durable run,
	// so it can be passed to an external service that drops duplicates. It
	// is empty when the run has no durable runner that supplies one.
	IdempotencyKey string
	// Approval says how the call was cleared to run: whether a gate or a
	// marker held it, who approved it, and which grant, if any, did so
	// without asking.
	Approval types.CallApproval
	// Knobs are the deployment's tool context (WithToolContext).
	Knobs types.ToolContext
}

// RunID returns the root run's ID.
func (rc RunContext[D]) RunID() string { return rc.Call.RunID }

// Branch returns the conversation branch the run extends.
func (rc RunContext[D]) Branch() types.BranchID { return rc.Call.Branch }

// NoDeps is the dependency type of a Func that needs none. A Func whose
// dependency type is an empty struct runs without deps attached.
type NoDeps = struct{}

type (
	agentDepsKey struct{}
	hostDepsKey  struct{}
)

// WithDeps sets the dependencies every Func tool of this agent receives as
// RunContext.Deps. Deps attached with ContextWithDeps take precedence.
func WithDeps(deps any) AgentOption {
	return func(c *AgentConfig) { c.Deps = deps }
}

// ContextWithDeps attaches dependencies to a run's context, for a host that
// builds them per request, such as per tenant. They reach every Func tool
// the run calls, including those of sub-agents, and take precedence over
// WithDeps.
func ContextWithDeps(ctx context.Context, deps any) context.Context {
	return context.WithValue(ctx, hostDepsKey{}, deps)
}

// depsFrom returns the dependencies attached to ctx as a D.
func depsFrom[D any](ctx context.Context) (D, error) {
	var zero D
	v := ctx.Value(hostDepsKey{})
	if v == nil {
		v = ctx.Value(agentDepsKey{})
	}
	if v == nil {
		if t := reflect.TypeFor[D](); t.Kind() == reflect.Struct && t.NumField() == 0 {
			return zero, nil
		}
		return zero, fmt.Errorf("no dependencies attached: want %T (use WithDeps or ContextWithDeps)", zero)
	}
	d, ok := v.(D)
	if !ok {
		return zero, fmt.Errorf("dependencies are %T, want %T", v, zero)
	}
	return d, nil
}

// NewRunContext builds the RunContext a Func tool would receive from ctx,
// for code that runs inside a tool call but is not a Func, and for tests.
func NewRunContext[D any](ctx context.Context) (RunContext[D], error) {
	deps, err := depsFrom[D](ctx)
	if err != nil {
		return RunContext[D]{}, err
	}
	info, _ := types.ToolCallInfoFromContext(ctx)
	ws, _ := workspace.FromContext(ctx)
	knobs := types.ToolContextFrom(ctx)
	return RunContext[D]{
		Context:        ctx,
		Deps:           deps,
		Call:           info,
		Workspace:      ws,
		IdempotencyKey: knobs.String(types.ToolContextIdempotencyKey, ""),
		Approval:       types.CallApprovalFrom(ctx),
		Knobs:          knobs,
	}, nil
}

// FuncOption configures a Func tool.
type FuncOption func(*funcConfig)

type funcConfig struct {
	capability types.ToolCapability
	markers    []types.Marker
	idempotent bool
	version    string
}

// Capability declares the tool's side-effect class, which CapabilityGate and
// an ApprovalPolicy judge it by. A Func that declares none is
// types.ToolCapabilityUnknown, which policies treat as the most dangerous
// class.
func Capability(c types.ToolCapability) FuncOption {
	return func(f *funcConfig) { f.capability = c }
}

// Markers attaches markers, so every call waits for their resolution before
// it runs, as for types.WithMarkers.
func Markers(markers ...types.Marker) FuncOption {
	return func(f *funcConfig) { f.markers = append(f.markers, markers...) }
}

// Approval requires a human approval before every call, with message shown
// to the person deciding. It is Markers with a "human_approval" marker.
func Approval(message string) FuncOption {
	return Markers(types.Marker{Kind: "human_approval", Message: message})
}

// Idempotent declares that repeating a call with the same input has the same
// effect as making it once (types.IdempotentTool). A durable engine may then
// repeat a call whose outcome a crash left unknown instead of waiting for
// the host to reconcile it.
func Idempotent() FuncOption {
	return func(f *funcConfig) { f.idempotent = true }
}

// withVersion overrides the schema-derived version.
func withVersion(v string) FuncOption {
	return func(f *funcConfig) { f.version = v }
}

// FuncTool is the tool Func builds. It implements types.VersionedTool and
// types.IdempotentTool.
type FuncTool struct {
	def        types.ToolDef
	version    string
	idempotent bool
	call       func(ctx context.Context, args map[string]any) (string, error)
}

var (
	_ types.VersionedTool  = (*FuncTool)(nil)
	_ types.IdempotentTool = (*FuncTool)(nil)
)

// Definition implements types.Tool.
func (t *FuncTool) Definition() types.ToolDef { return t.def }

// Execute implements types.Tool.
func (t *FuncTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	return t.call(ctx, args)
}

// Version is a hash of the tool's definition (types.DefinitionHash), so it
// changes whenever the input type, the name, the description or the
// capability changes. An AIFunc tool reports the AIFunc's version instead.
func (t *FuncTool) Version() string { return t.version }

// Idempotent implements types.IdempotentTool.
func (t *FuncTool) Idempotent() bool { return t.idempotent }

// Func builds a tool from a typed Go function.
//
// The parameter schema is derived from In with types.SchemaFrom, so In must
// be a struct; its json, description and enum tags shape the schema. Each
// call's arguments are decoded strictly into In: an unknown property or a
// value of the wrong type is an invalid-arguments tool error
// (types.ErrInvalidToolArguments) the model can correct, as for any tool
// whose arguments do not fit its schema. A string Out is returned as is;
// any other Out is encoded as JSON.
//
// fn receives a RunContext with the host's dependencies as a D. A call that
// finds no dependencies of that type fails with a tool error, unless D is an
// empty struct such as NoDeps.
//
// The tool is wrapped in a types.MarkedTool when Markers or Approval is set.
// Func panics when In is not a struct, because no schema can be derived.
func Func[D, In, Out any](name, description string, fn func(RunContext[D], In) (Out, error), opts ...FuncOption) types.Tool {
	if t := reflect.TypeFor[In](); t.Kind() != reflect.Struct && (t.Kind() != reflect.Pointer || t.Elem().Kind() != reflect.Struct) {
		panic(fmt.Sprintf("agent.Func %q: input type %s is not a struct", name, t))
	}
	var cfg funcConfig
	for _, o := range opts {
		o(&cfg)
	}
	def := types.ToolDef{
		Name:        name,
		Description: description,
		Parameters:  types.SchemaFrom[In](),
		Capability:  cfg.capability,
	}
	version := cfg.version
	if version == "" {
		version = types.DefinitionHash(def)
	}
	tool := &FuncTool{
		def:        def,
		version:    version,
		idempotent: cfg.idempotent,
		call: func(ctx context.Context, args map[string]any) (string, error) {
			in, err := DecodeArgs[In](args)
			if err != nil {
				return "", err
			}
			rc, err := NewRunContext[D](ctx)
			if err != nil {
				return "", err
			}
			out, err := fn(rc, in)
			if err != nil {
				return "", err
			}
			return encodeOutput(out)
		},
	}
	if len(cfg.markers) > 0 {
		return types.WithMarkers(tool, cfg.markers...)
	}
	return tool
}

// DecodeArgs decodes tool arguments into T strictly: a property T does not
// declare, or a value that does not fit its field, is an error wrapping
// types.ErrInvalidToolArguments.
func DecodeArgs[T any](args map[string]any) (T, error) {
	var v T
	raw, err := json.Marshal(args)
	if err != nil {
		return v, fmt.Errorf("%w: %w", types.ErrInvalidToolArguments, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, fmt.Errorf("%w: %w", types.ErrInvalidToolArguments, err)
	}
	return v, nil
}

// encodeOutput returns a string as is and encodes anything else as JSON.
func encodeOutput(out any) (string, error) {
	if s, ok := out.(string); ok {
		return s, nil
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("encode result: %w", err)
	}
	return string(raw), nil
}
