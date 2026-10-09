package selector

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/types"
)

// ToolSearchName is the name of the discovery tool DeferredTools provides.
const ToolSearchName = "tool_search"

// Defaults for DeferredTools.
const (
	DefaultSearchResults = 5
	MaxSearchResults     = 20
	DefaultMaxScopes     = 1024
)

// DeferredTools is an agent.ToolPolicy that sends the model only a small set
// of tools and lets it find the rest with a search tool.
//
// Each turn the model sees the pinned tools, the tool_search tool, and any
// tools it has already discovered. tool_search(query, k) ranks the hidden
// tools, returns their full schemas, and discovers them: from the next turn
// they are sent like any other tool. Discovery lasts for the rest of the
// conversation (see agent.RunScope), so a schema found once is not searched
// for again.
//
// Register Tool() on the agent next to the tools it hides. When the turn's
// tools do not include tool_search (for example in a sub-agent that inherits
// this policy but not the tool), nothing is hidden, because a hidden tool
// with no way to find it is unreachable.
//
// Selecting tools is disclosure, not permission. A discovered tool still
// passes through the agent's ToolGate on every call.
//
// Discovery is held in process memory. A run resumed in another process
// starts with only the pinned tools again; the model can search again.
type DeferredTools struct {
	// Pinned names tools that are always sent.
	Pinned []string
	// Search ranks hidden tools for tool_search. nil uses BM25 over each
	// tool's name, description, and parameter names and descriptions.
	Search Selector[types.ToolDef]
	// DefaultResults is the k used when the model omits it. Zero uses
	// DefaultSearchResults.
	DefaultResults int
	// MaxResults caps the k the model may ask for. Zero uses
	// MaxSearchResults.
	MaxResults int
	// MaxScopes bounds how many conversations keep discovery state; the
	// oldest is forgotten first. Zero uses DefaultMaxScopes.
	MaxScopes int

	mu     sync.Mutex
	scopes map[string]*deferredScope
	order  []string
}

type deferredScope struct {
	defs       []types.ToolDef // the turn's full tool set, as last selected
	discovered map[string]bool
}

// NewDeferredTools returns a policy that always sends the pinned tools.
func NewDeferredTools(pinned ...string) *DeferredTools {
	return &DeferredTools{Pinned: pinned}
}

// ToolDefText is the searchable text BM25 uses for a tool by default.
func ToolDefText(def types.ToolDef) string {
	var b strings.Builder
	// The name is the strongest signal, so it is counted twice.
	b.WriteString(def.Name + " " + def.Name + " " + def.Description)
	names := make([]string, 0, len(def.Parameters.Properties))
	for name := range def.Parameters.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		b.WriteString(" " + name + " " + def.Parameters.Properties[name].Description)
	}
	return b.String()
}

// Select implements agent.ToolPolicy.
func (d *DeferredTools) Select(ctx context.Context, owner string, defs []types.ToolDef) ([]string, error) {
	hasSearch := slices.ContainsFunc(defs, func(def types.ToolDef) bool { return def.Name == ToolSearchName })
	names := make([]string, 0, len(defs))
	if !hasSearch {
		for _, def := range defs {
			names = append(names, def.Name)
		}
		return names, nil
	}

	key := scopeKey(ctx, owner)
	d.mu.Lock()
	defer d.mu.Unlock()
	scope := d.scopeLocked(key)
	scope.defs = slices.Clone(defs)
	for _, def := range defs {
		if d.visibleLocked(scope, def.Name) {
			names = append(names, def.Name)
		}
	}
	return names, nil
}

// Discovered returns the tools discovered in a conversation, sorted.
func (d *DeferredTools) Discovered(scope agent.RunScope) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.scopes[scope.Key()]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(s.discovered))
	for name := range s.discovered {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Forget drops a conversation's discovery state.
func (d *DeferredTools) Forget(scope agent.RunScope) {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := scope.Key()
	delete(d.scopes, key)
	d.order = slices.DeleteFunc(d.order, func(k string) bool { return k == key })
}

// Tool returns the tool_search tool bound to this policy.
func (d *DeferredTools) Tool() types.Tool { return &toolSearch{policy: d} }

func (d *DeferredTools) visibleLocked(scope *deferredScope, name string) bool {
	return name == ToolSearchName || slices.Contains(d.Pinned, name) || scope.discovered[name]
}

func (d *DeferredTools) scopeLocked(key string) *deferredScope {
	if s, ok := d.scopes[key]; ok {
		return s
	}
	if d.scopes == nil {
		d.scopes = map[string]*deferredScope{}
	}
	limit := d.MaxScopes
	if limit <= 0 {
		limit = DefaultMaxScopes
	}
	for len(d.order) >= limit {
		delete(d.scopes, d.order[0])
		d.order = d.order[1:]
	}
	s := &deferredScope{discovered: map[string]bool{}}
	d.scopes[key] = s
	d.order = append(d.order, key)
	return s
}

func scopeKey(ctx context.Context, owner string) string {
	if scope, ok := agent.RunScopeFromContext(ctx); ok {
		return scope.Key()
	}
	return agent.RunScope{Agent: owner}.Key()
}

// search ranks the hidden tools of the caller's conversation and discovers
// the matches.
func (d *DeferredTools) search(ctx context.Context, query string, k int) ([]types.ToolDef, error) {
	scope, _ := agent.RunScopeFromContext(ctx)
	key := scope.Key()

	d.mu.Lock()
	s, ok := d.scopes[key]
	var hidden []types.ToolDef
	if ok {
		for _, def := range s.defs {
			if !d.visibleLocked(s, def.Name) {
				hidden = append(hidden, def)
			}
		}
	}
	d.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%s has no tool list for this conversation; it must run inside an agent that uses this policy", ToolSearchName)
	}

	sel := d.Search
	if sel == nil {
		sel = NewBM25(ToolDefText)
	}
	found, err := sel.Select(ctx, query, hidden, k)
	if err != nil {
		return nil, err
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	// The scope may have been evicted or forgotten while ranking ran; the
	// result is still returned, and discovery is recorded if it still exists.
	if s, ok := d.scopes[key]; ok {
		for _, def := range found {
			s.discovered[def.Name] = true
		}
	}
	return found, nil
}

type toolSearch struct{ policy *DeferredTools }

func (t *toolSearch) Definition() types.ToolDef {
	return types.ToolDef{
		Name: ToolSearchName,
		Description: "Search for tools that are available but not yet loaded. " +
			"Returns the full definitions of the best matches; matched tools can be called from the next turn on.",
		Parameters: types.ParameterSchema{
			Type:     types.SchemaObject,
			Required: []string{"query"},
			Properties: map[string]types.PropertyDef{
				"query": {Type: types.SchemaString, Description: "Keywords describing the capability you need"},
				"k":     {Type: types.SchemaInteger, Description: "How many tools to return", Nullable: true},
			},
		},
		Capability: types.ToolCapabilityRead,
	}
}

func (t *toolSearch) Execute(ctx context.Context, args map[string]any) (string, error) {
	query, _ := args["query"].(string)
	k := t.policy.DefaultResults
	if k <= 0 {
		k = DefaultSearchResults
	}
	if n, ok := intArg(args["k"]); ok && n > 0 {
		k = n
	}
	limit := t.policy.MaxResults
	if limit <= 0 {
		limit = MaxSearchResults
	}
	k = min(k, limit)

	found, err := t.policy.search(ctx, query, k)
	if err != nil {
		return "", err
	}
	if len(found) == 0 {
		return "No matching tools. Try different keywords.", nil
	}
	// Only what the model needs to call the tool: the capability class is
	// policy metadata and is never shown to the model.
	type schema struct {
		Name        string                `json:"name"`
		Description string                `json:"description,omitempty"`
		Parameters  types.ParameterSchema `json:"parameters"`
	}
	schemas := make([]schema, len(found))
	for i, def := range found {
		schemas[i] = schema{Name: def.Name, Description: def.Description, Parameters: def.Parameters}
	}
	out, err := json.Marshal(struct {
		Tools []schema `json:"tools"`
	}{schemas})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// intArg reads a numeric tool argument, which may arrive as a float64 from
// JSON decoding or as a json.Number.
func intArg(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}
