// Package discovery is how an agent finds and loads what it is not shown up
// front: tools held back from its tool list, skills, and the agents it may
// delegate to. Two tools do it, the same way for every kind:
//
//   - discover(query, kind, k) ranks what the agent may load by name,
//     description and text (a skill's instructions, an agent's system
//     prompt), and answers with each match's kind, name, description and an
//     excerpt of its text;
//   - load(kind, name) loads one: a held tool becomes callable from the next
//     turn and its schema is returned, a skill's instructions are returned,
//     and an agent's description and system prompt are returned so the model
//     can decide whether to delegate to it.
//
// Loading is disclosure, not permission. A loaded tool still passes the
// agent's ToolGate on every call, and a skill or an agent's instructions do
// not change what the agent may do.
//
// Which tools a conversation has loaded is kept in a Store. The default
// keeps it in process memory; a host that resumes runs in another process
// passes a durable Store so a resumed run keeps what it loaded.
package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/selector"
	"github.com/urmzd/saige/agent/types"
)

// Names of the discovery tools.
const (
	DiscoverName = "discover"
	LoadName     = "load"
)

// Argument names of the discovery tools.
const (
	argQuery = "query"
	argKind  = "kind"
	argName  = "name"
	argK     = "k"
)

// Kind is what an entry is.
type Kind string

// Kinds of entries.
const (
	KindTool  Kind = "tool"
	KindSkill Kind = "skill"
	KindAgent Kind = "agent"
)

// Kinds lists every kind, in documentation order.
func Kinds() []Kind { return []Kind{KindTool, KindSkill, KindAgent} }

// Defaults for a Discovery.
const (
	DefaultResults   = 5
	MaxResults       = 20
	DefaultMaxScopes = 1024
	// ExcerptLen bounds the excerpt of an entry's text in a discover answer.
	ExcerptLen = 240
)

// Entry is one skill or agent the model may discover and load. Held tools
// are entries too, built from the turn's tool list.
type Entry struct {
	Kind        Kind
	Name        string
	Description string
	// Text is the rest of what discover searches, and what load returns
	// when Load is nil: a skill's instructions, an agent's system prompt.
	Text string
	// Load, when set, answers load for this entry, such as a skill toolset
	// that also records the active skill.
	Load func(ctx context.Context) (string, error)
}

// Discovery is the policy and the two tools for one agent. One Discovery
// serves many conversations; per-conversation state is keyed by
// agent.RunScope.
type Discovery struct {
	// Owner is the agent the Discovery serves. A sub-agent inherits its
	// parent's ToolPolicy, so the policy passes another agent's turns
	// through unchanged: a parent's held tools are the parent's alone.
	// Empty serves every agent. Set it when the tools may run without an
	// agent.RunScope in their context, so the policy and the tools share
	// the owner's key.
	Owner string
	// Hold reports whether a tool is held back from the tool list until it
	// is loaded. Nil holds none.
	Hold func(name string) bool
	// Entries are the skills and agents discover searches and load loads.
	Entries []Entry
	// Store keeps the tools each conversation loaded. Nil keeps them in
	// memory.
	Store Store
	// Results is the k discover uses when the model gives none. Zero uses
	// DefaultResults.
	Results int
	// MaxScopes bounds how many conversations keep a tool list for
	// discover; the one used longest ago is forgotten first. Zero uses
	// DefaultMaxScopes.
	MaxScopes int

	once   sync.Once
	mem    *MemoryStore
	mu     sync.Mutex
	scopes map[string][]types.ToolDef
	order  []string
}

// New returns a Discovery for owner that holds the tools hold names and
// offers entries.
func New(owner string, hold func(name string) bool, entries ...Entry) *Discovery {
	return &Discovery{Owner: owner, Hold: hold, Entries: entries}
}

// Tools returns discover and load.
func (d *Discovery) Tools() []types.Tool {
	return []types.Tool{&discoverTool{d}, &loadTool{d}}
}

// Policy returns a ToolPolicy that offers what inner offers, less the held
// tools the conversation has not loaded. A nil inner offers every tool.
func (d *Discovery) Policy(inner agent.ToolPolicy) agent.ToolPolicy {
	return agent.ToolPolicyFunc(func(ctx context.Context, owner string, defs []types.ToolDef) ([]string, error) {
		names := make([]string, 0, len(defs))
		if inner != nil {
			picked, err := inner.Select(ctx, owner, defs)
			if err != nil {
				return nil, err
			}
			names = picked
		} else {
			for _, def := range defs {
				names = append(names, def.Name)
			}
		}
		if d.Owner != "" && owner != d.Owner {
			return names, nil
		}
		// discover searches what inner offers, so it never reveals a tool
		// another policy hides, such as one a loaded skill narrows away.
		offered := make([]types.ToolDef, 0, len(names))
		for _, def := range defs {
			if slices.Contains(names, def.Name) {
				offered = append(offered, def)
			}
		}
		key := d.scopeKey(ctx, owner)
		d.remember(key, offered)
		loaded, err := d.store().Loaded(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("discovery: loaded tools: %w", err)
		}
		names = slices.DeleteFunc(names, func(name string) bool {
			return d.held(name) && !slices.Contains(loaded, name)
		})
		// discover and load stay offered whatever inner narrows, as a
		// loaded skill's allowed-tools keeps its own skill tools.
		for _, def := range defs {
			if (def.Name == DiscoverName || def.Name == LoadName) && !slices.Contains(names, def.Name) {
				names = append(names, def.Name)
			}
		}
		return names, nil
	})
}

// Loaded returns the tools a conversation loaded, sorted.
func (d *Discovery) Loaded(ctx context.Context, scope agent.RunScope) ([]string, error) {
	out, err := d.store().Loaded(ctx, scope.Key())
	if err != nil {
		return nil, err
	}
	out = slices.Clone(out)
	sort.Strings(out)
	return out, nil
}

func (d *Discovery) store() Store {
	if d.Store != nil {
		return d.Store
	}
	d.once.Do(func() { d.mem = &MemoryStore{MaxScopes: d.MaxScopes} })
	return d.mem
}

func (d *Discovery) held(name string) bool {
	if name == DiscoverName || name == LoadName || d.Hold == nil {
		return false
	}
	return d.Hold(name)
}

func (d *Discovery) remember(key string, defs []types.ToolDef) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.scopes == nil {
		d.scopes = map[string][]types.ToolDef{}
	}
	if _, ok := d.scopes[key]; !ok {
		limit := d.MaxScopes
		if limit <= 0 {
			limit = DefaultMaxScopes
		}
		for len(d.order) >= limit {
			delete(d.scopes, d.order[0])
			d.order = d.order[1:]
		}
		d.order = append(d.order, key)
	} else if i := slices.Index(d.order, key); i >= 0 {
		d.order = append(slices.Delete(d.order, i, i+1), key)
	}
	d.scopes[key] = slices.Clone(defs)
}

// heldTools are the conversation's held tools that it has not loaded.
func (d *Discovery) heldTools(ctx context.Context, key string) ([]types.ToolDef, error) {
	d.mu.Lock()
	defs, ok := d.scopes[key]
	d.mu.Unlock()
	if !ok {
		return nil, nil
	}
	loaded, err := d.store().Loaded(ctx, key)
	if err != nil {
		return nil, err
	}
	var out []types.ToolDef
	for _, def := range defs {
		if d.held(def.Name) && !slices.Contains(loaded, def.Name) {
			out = append(out, def)
		}
	}
	return out, nil
}

// toolDef finds a tool in the conversation's tool list.
func (d *Discovery) toolDef(key, name string) (types.ToolDef, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, def := range d.scopes[key] {
		if def.Name == name {
			return def, true
		}
	}
	return types.ToolDef{}, false
}

// candidates are the entries discover ranks for the conversation, of kind
// when it is set.
func (d *Discovery) candidates(ctx context.Context, key string, kind Kind) ([]Entry, error) {
	var out []Entry
	if kind == "" || kind == KindTool {
		held, err := d.heldTools(ctx, key)
		if err != nil {
			return nil, err
		}
		for _, def := range held {
			out = append(out, Entry{Kind: KindTool, Name: def.Name, Description: def.Description, Text: paramText(def)})
		}
	}
	for _, e := range d.Entries {
		if kind == "" || e.Kind == kind {
			out = append(out, e)
		}
	}
	return out, nil
}

// scopeKey is the conversation's key: its RunScope, or, outside an agent
// loop, the owner's. The policy and the tools fall back to the same key.
func (d *Discovery) scopeKey(ctx context.Context, owner string) string {
	if scope, ok := agent.RunScopeFromContext(ctx); ok {
		return scope.Key()
	}
	if owner == "" {
		owner = d.Owner
	}
	return agent.RunScope{Agent: owner}.Key()
}

// entryText is the text BM25 ranks an entry by. The name is the strongest
// signal, so it is counted twice.
func entryText(e Entry) string {
	return e.Name + " " + e.Name + " " + e.Description + " " + e.Text
}

// paramText is a tool's parameter names and descriptions.
func paramText(def types.ToolDef) string {
	names := make([]string, 0, len(def.Parameters.Properties))
	for name := range def.Parameters.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		b.WriteString(name + " " + def.Parameters.Properties[name].Description + " ")
	}
	return strings.TrimSpace(b.String())
}

// excerpt is up to ExcerptLen characters of text around the first query
// term it contains, or its start. It searches rune by rune, lowercased one
// rune at a time, so the position found is the position in text.
func excerpt(text, query string) string {
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= ExcerptLen {
		return text
	}
	lower := make([]rune, len(runes))
	for i, r := range runes {
		lower[i] = unicode.ToLower(r)
	}
	at := -1
	for _, term := range selector.Tokenize(query) {
		if i := runeIndex(lower, []rune(term)); i >= 0 && (at < 0 || i < at) {
			at = i
		}
	}
	start := 0
	if at > 0 {
		start = max(0, at-ExcerptLen/4)
	}
	end := min(len(runes), start+ExcerptLen)
	out := string(runes[start:end])
	if start > 0 {
		out = "…" + out
	}
	if end < len(runes) {
		out += "…"
	}
	return out
}

// runeIndex is the index of the first sub in s, or -1.
func runeIndex(s, sub []rune) int {
	if len(sub) == 0 {
		return -1
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if slices.Equal(s[i:i+len(sub)], sub) {
			return i
		}
	}
	return -1
}

type discoverTool struct{ d *Discovery }

func (t *discoverTool) Definition() types.ToolDef {
	kinds := make([]string, 0, len(Kinds()))
	for _, k := range Kinds() {
		kinds = append(kinds, string(k))
	}
	return types.ToolDef{
		Name: DiscoverName,
		Description: "Search the tools, skills and agents you can load but are not shown in full. " +
			"Matches are ranked by name, description and instructions. Load one with " + LoadName + ".",
		Parameters: types.ParameterSchema{
			Type:     types.SchemaObject,
			Required: []string{argQuery},
			Properties: map[string]types.PropertyDef{
				argQuery: {Type: types.SchemaString, Description: "Keywords describing what you need. Empty lists everything of kind."},
				argKind:  {Type: types.SchemaString, Description: "Only this kind", Enum: kinds, Nullable: true},
				argK:     {Type: types.SchemaInteger, Description: "How many matches to return", Nullable: true},
			},
		},
		Capability: types.ToolCapabilityRead,
	}
}

// match is one discover result.
type match struct {
	Kind        Kind   `json:"kind"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Excerpt     string `json:"excerpt,omitempty"`
}

func (t *discoverTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	query, _ := args[argQuery].(string)
	kind, err := kindArg(args)
	if err != nil {
		return "", err
	}
	k := t.d.Results
	if k <= 0 {
		k = DefaultResults
	}
	if n, ok := intArg(args[argK]); ok && n > 0 {
		k = n
	}
	k = min(k, MaxResults)

	all, err := t.d.candidates(ctx, t.d.scopeKey(ctx, ""), kind)
	if err != nil {
		return "", err
	}
	var found []Entry
	if strings.TrimSpace(query) == "" || len(selector.Tokenize(query)) == 0 {
		found = slices.Clone(all)
		slices.SortStableFunc(found, func(a, b Entry) int {
			if c := strings.Compare(string(a.Kind), string(b.Kind)); c != 0 {
				return c
			}
			return strings.Compare(a.Name, b.Name)
		})
		found = found[:min(len(found), k)]
	} else if found, err = selector.NewBM25(entryText).Select(ctx, query, all, k); err != nil {
		return "", err
	}
	if len(found) == 0 {
		return "Nothing matches. Try other keywords, or an empty query to list everything.", nil
	}
	out := make([]match, len(found))
	for i, e := range found {
		out[i] = match{Kind: e.Kind, Name: e.Name, Description: strings.Join(strings.Fields(e.Description), " "), Excerpt: excerpt(e.Text, query)}
	}
	b, err := json.Marshal(struct {
		Matches []match `json:"matches"`
	}{out})
	return string(b), err
}

type loadTool struct{ d *Discovery }

func (t *loadTool) Definition() types.ToolDef {
	kinds := make([]string, 0, len(Kinds()))
	for _, k := range Kinds() {
		kinds = append(kinds, string(k))
	}
	return types.ToolDef{
		Name: LoadName,
		Description: "Load one tool, skill or agent by kind and name. A tool becomes callable from your next turn " +
			"and its schema is returned. A skill returns its instructions. An agent returns its description and " +
			"instructions, so you can decide whether to delegate to it. Loading does not change what you are permitted to do.",
		Parameters: types.ParameterSchema{
			Type:     types.SchemaObject,
			Required: []string{argKind, argName},
			Properties: map[string]types.PropertyDef{
				argKind: {Type: types.SchemaString, Description: "What to load", Enum: kinds},
				argName: {Type: types.SchemaString, Description: "Its name, as discover returned it"},
			},
		},
		Capability: types.ToolCapabilityRead,
	}
}

func (t *loadTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	kind, err := kindArg(args)
	if err != nil {
		return "", err
	}
	if kind == "" {
		return "", fmt.Errorf("%s: kind is required", LoadName)
	}
	name, _ := args[argName].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("%s: name is required", LoadName)
	}
	key := t.d.scopeKey(ctx, "")
	if kind == KindTool {
		return t.loadTool(ctx, key, name)
	}
	for _, e := range t.d.Entries {
		if e.Kind != kind || e.Name != name {
			continue
		}
		if e.Load != nil {
			return e.Load(ctx)
		}
		return describe(e), nil
	}
	return "", fmt.Errorf("%s: no %s named %q; find one with %s", LoadName, kind, name, DiscoverName)
}

func (t *loadTool) loadTool(ctx context.Context, key, name string) (string, error) {
	def, ok := t.d.toolDef(key, name)
	if !ok {
		return "", fmt.Errorf("%s: no tool named %q; find one with %s", LoadName, name, DiscoverName)
	}
	if t.d.held(name) {
		if err := t.d.store().Add(ctx, key, name); err != nil {
			return "", fmt.Errorf("discovery: record loaded tool: %w", err)
		}
	}
	// Only what the model needs to call the tool: the capability class is
	// policy metadata and is never shown to the model.
	b, err := json.Marshal(struct {
		Name        string                `json:"name"`
		Description string                `json:"description,omitempty"`
		Parameters  types.ParameterSchema `json:"parameters"`
		Note        string                `json:"note"`
	}{def.Name, def.Description, def.Parameters, "Callable from your next turn."})
	return string(b), err
}

// describe is what load answers for an entry without its own Load.
func describe(e Entry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", e.Kind, e.Name)
	if d := strings.TrimSpace(e.Description); d != "" {
		b.WriteString(": " + strings.Join(strings.Fields(d), " "))
	}
	if text := strings.TrimSpace(e.Text); text != "" {
		b.WriteString("\n\n" + text)
	}
	return b.String()
}

func kindArg(args map[string]any) (Kind, error) {
	raw, _ := args[argKind].(string)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if !slices.Contains(Kinds(), Kind(raw)) {
		return "", fmt.Errorf("kind %q is not one of tool, skill or agent", raw)
	}
	return Kind(raw), nil
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
