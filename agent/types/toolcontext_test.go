package types

import (
	"context"
	"testing"
)

// configurableTool reads a knob and a dependency, and reports what it saw.
type configurableTool struct {
	limit int
	db    string
}

func (c *configurableTool) Definition() ToolDef { return ToolDef{Name: "search"} }
func (c *configurableTool) Execute(context.Context, map[string]any) (string, error) {
	return c.db + ":" + itoa(c.limit), nil
}
func (c *configurableTool) ContextSchema() ParameterSchema {
	return ParameterSchema{Type: "object", Properties: map[string]PropertyDef{
		"limit": {Type: "integer", Description: "max results"},
	}}
}
func (c *configurableTool) Requires() []string { return []string{"db"} }
func (c *configurableTool) Configure(tc ToolContext, deps Deps) (Tool, error) {
	db, err := Dep[string](deps, "db")
	if err != nil {
		return nil, err
	}
	return &configurableTool{limit: tc.Int("limit", 10), db: db}, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// Immutability is not cosmetic: tool calls are fanned out, so a shared mutable
// map would give two parallel calls different configuration with no ordering
// guarantee about which won.
func TestToolContextIsImmutable(t *testing.T) {
	base := NewToolContext(map[string]any{"limit": 10})
	derived := base.With("limit", 50).With("root", "/tmp")

	if base.Int("limit", 0) != 10 {
		t.Error("With must not mutate the receiver")
	}
	if derived.Int("limit", 0) != 50 || derived.String("root", "") != "/tmp" {
		t.Errorf("derived context = %+v, want the overrides applied", derived.Keys())
	}
	if base.Len() != 1 {
		t.Errorf("base has %d keys, want 1", base.Len())
	}
}

func TestSourceMapIsCopiedOnConstruction(t *testing.T) {
	src := map[string]any{"limit": 10}
	tc := NewToolContext(src)
	src["limit"] = 999

	if tc.Int("limit", 0) != 10 {
		t.Error("mutating the source map after construction must not reach the tool")
	}
}

// A misconfigured knob degrades to the default rather than failing a tool call
// the model is waiting on.
func TestTypedAccessorsFallBackOnWrongTypes(t *testing.T) {
	tc := NewToolContext(map[string]any{
		"limit": "not a number",
		"ratio": "not a float",
		"on":    "not a bool",
		"name":  42,
	})
	if got := tc.Int("limit", 7); got != 7 {
		t.Errorf("Int = %d, want the default 7", got)
	}
	if got := tc.Float("ratio", 1.5); got != 1.5 {
		t.Errorf("Float = %v, want the default", got)
	}
	if got := tc.Bool("on", true); !got {
		t.Error("Bool must fall back to the default")
	}
	if got := tc.String("name", "fallback"); got != "fallback" {
		t.Errorf("String = %q, want the default", got)
	}
	if got := tc.Int("absent", 3); got != 3 {
		t.Errorf("Int on a missing key = %d, want 3", got)
	}
}

// JSON config decodes numbers as float64; a knob loaded from a file must still
// read as an int.
func TestIntAcceptsJSONDecodedNumbers(t *testing.T) {
	tc := NewToolContext(map[string]any{"limit": float64(25)})
	if got := tc.Int("limit", 0); got != 25 {
		t.Errorf("Int = %d, want 25 from a JSON-decoded float", got)
	}
}

func TestMergeAppliesTheOtherOnTop(t *testing.T) {
	base := NewToolContext(map[string]any{"limit": 10, "root": "/a"})
	override := NewToolContext(map[string]any{"limit": 99})
	merged := base.Merge(override)

	if merged.Int("limit", 0) != 99 {
		t.Error("the merged-in value must win")
	}
	if merged.String("root", "") != "/a" {
		t.Error("keys absent from the override must survive")
	}
}

func TestToolContextTravelsThroughContext(t *testing.T) {
	tc := NewToolContext(map[string]any{"root": "/srv"})
	ctx := WithToolContext(context.Background(), tc)

	if got := ToolContextFrom(ctx).String("root", ""); got != "/srv" {
		t.Errorf("root = %q, want /srv", got)
	}
	// Never nil, so a tool can call accessors unconditionally.
	if got := ToolContextFrom(context.Background()).String("root", "default"); got != "default" {
		t.Errorf("a context with no ToolContext must yield an empty one, got %q", got)
	}
}

func TestDepReportsMissingAndMistypedDependencies(t *testing.T) {
	deps := NewDeps(map[string]any{"db": "postgres://x", "count": 5})

	if got, err := Dep[string](deps, "db"); err != nil || got != "postgres://x" {
		t.Errorf("Dep = %q, %v", got, err)
	}
	if _, err := Dep[string](deps, "absent"); err == nil {
		t.Error("a missing dependency must error, not return a zero value that panics later")
	}
	if _, err := Dep[string](deps, "count"); err == nil {
		t.Error("a dependency of the wrong type must error at lookup, not panic mid-run")
	}
}

func TestConfigureBindsContextAndDependencies(t *testing.T) {
	proto := &configurableTool{}
	tc := NewToolContext(map[string]any{"limit": 50})
	deps := NewDeps(map[string]any{"db": "postgres://prod"})

	bound, err := Configure(proto, tc, deps)
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	out, _ := bound.Execute(context.Background(), nil)
	if out != "postgres://prod:50" {
		t.Errorf("bound tool produced %q, want the configured knob and dependency", out)
	}

	// The prototype is untouched, so one registration can produce many
	// differently-configured instances.
	if proto.limit != 0 || proto.db != "" {
		t.Error("Configure must not mutate the prototype")
	}
}

// Wiring must fail loudly at construction rather than nil-panicking on the
// first call.
func TestConfigureFailsOnMissingDependencies(t *testing.T) {
	_, err := Configure(&configurableTool{}, ToolContext{}, NewDeps(nil))
	if err == nil {
		t.Fatal("a Configurable with unmet dependencies must fail at wiring time")
	}
}

func TestConfigureLeavesPlainToolsAlone(t *testing.T) {
	plain := &ToolFunc{Def: ToolDef{Name: "plain"}}
	got, err := Configure(plain, ToolContext{}, NewDeps(nil))
	if err != nil {
		t.Fatal(err)
	}
	if got != Tool(plain) {
		t.Error("a tool that is not Configurable must pass through unchanged")
	}
}

func TestConfigureAllRebuildsARegistry(t *testing.T) {
	src := NewToolRegistry(&configurableTool{}, &ToolFunc{Def: ToolDef{Name: "plain"}})
	deps := NewDeps(map[string]any{"db": "sqlite://x"})

	out, err := ConfigureAll(src, NewToolContext(map[string]any{"limit": 3}), deps)
	if err != nil {
		t.Fatalf("ConfigureAll: %v", err)
	}
	tool, _ := out.Get("search")
	res, _ := tool.Execute(context.Background(), nil)
	if res != "sqlite://x:3" {
		t.Errorf("configured tool produced %q", res)
	}
	if _, ok := out.Get("plain"); !ok {
		t.Error("non-configurable tools must survive the rebuild")
	}
}

func TestCachePolicyValidation(t *testing.T) {
	tests := []struct {
		name   string
		policy CachePolicy
		ok     bool
	}{
		{"disabled is always valid", CachePolicy{}, true},
		{"enabled needs a TTL", CachePolicy{Enabled: true}, false},
		{"minimal valid", CachePolicy{Enabled: true, TTL: 1}, true},
		{"stale needs a bound", CachePolicy{Enabled: true, TTL: 1, ServeStaleOnError: true}, false},
		{"stale with bound", CachePolicy{Enabled: true, TTL: 1, ServeStaleOnError: true, MaxStale: 2}, true},
		{"contradictory key rules", CachePolicy{
			Enabled: true, TTL: 1, KeyArgs: []string{"q"}, IgnoreArgs: []string{"q"},
		}, false},
		{"unknown scope", CachePolicy{Enabled: true, TTL: 1, Scope: "planet"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.policy.Validate(); (err == nil) != tt.ok {
				t.Errorf("Validate = %v, want ok=%v", err, tt.ok)
			}
		})
	}
}

// Map iteration order must never produce two keys for one call.
func TestKeyArgumentsAreDeterministic(t *testing.T) {
	p := CachePolicy{Enabled: true, TTL: 1, IgnoreArgs: []string{"trace"}}
	args := map[string]any{"z": 1, "a": 2, "m": 3, "trace": "abc"}

	first := p.KeyArguments(args)
	for i := 0; i < 20; i++ {
		got := p.KeyArguments(args)
		if len(got) != len(first) {
			t.Fatalf("key arguments length varies: %v vs %v", got, first)
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("key argument order varies: %v vs %v", got, first)
			}
		}
	}
	for _, name := range first {
		if name == "trace" {
			t.Error("an ignored argument must not appear in the key")
		}
	}
}

func TestPolicyForReportsUncachedForPlainTools(t *testing.T) {
	if got := PolicyFor(&ToolFunc{Def: ToolDef{Name: "x"}}); got.Enabled {
		t.Error("a tool that declares nothing must be uncached")
	}
}
