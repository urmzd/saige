package types

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestToolFunc(t *testing.T) {
	tool := &ToolFunc{
		Def: ToolDef{
			Name:        "greet",
			Description: "Greet a person",
			Parameters: ParameterSchema{
				Type:     "object",
				Required: []string{"name"},
				Properties: map[string]PropertyDef{
					"name": {Type: "string", Description: "Person's name"},
				},
			},
		},
		Fn: func(ctx context.Context, args map[string]any) (string, error) {
			return "Hello, " + args["name"].(string) + "!", nil
		},
	}

	def := tool.Definition()
	if def.Name != "greet" {
		t.Errorf("Name = %q, want greet", def.Name)
	}

	result, err := tool.Execute(context.Background(), map[string]any{"name": "Alice"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result != "Hello, Alice!" {
		t.Errorf("result = %q, want %q", result, "Hello, Alice!")
	}
}

func TestToolRegistry(t *testing.T) {
	tool1 := &ToolFunc{
		Def: ToolDef{Name: "tool1"},
		Fn:  func(ctx context.Context, args map[string]any) (string, error) { return "1", nil },
	}
	tool2 := &ToolFunc{
		Def: ToolDef{Name: "tool2"},
		Fn:  func(ctx context.Context, args map[string]any) (string, error) { return "2", nil },
	}

	reg := NewToolRegistry(tool1)

	// Get existing
	got, ok := reg.Get("tool1")
	if !ok {
		t.Fatal("tool1 not found")
	}
	if got.Definition().Name != "tool1" {
		t.Error("wrong tool returned")
	}

	// Get missing
	_, ok = reg.Get("nonexistent")
	if ok {
		t.Error("expected not found")
	}

	// Register and retrieve
	reg.Register(tool2)
	defs := reg.Definitions()
	if len(defs) != 2 {
		t.Errorf("Definitions len = %d, want 2", len(defs))
	}
}

func TestToolRegistryExecute(t *testing.T) {
	tool := &ToolFunc{
		Def: ToolDef{Name: "echo"},
		Fn: func(ctx context.Context, args map[string]any) (string, error) {
			return args["msg"].(string), nil
		},
	}
	reg := NewToolRegistry(tool)

	result, err := reg.Execute(context.Background(), "echo", map[string]any{"msg": "hi"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result != "hi" {
		t.Errorf("result = %q", result)
	}

	// Execute missing tool
	_, err = reg.Execute(context.Background(), "missing", nil)
	if !errors.Is(err, ErrToolNotFound) {
		t.Errorf("err = %v, want ErrToolNotFound", err)
	}
}

func TestToolRegistryConcurrency(t *testing.T) {
	reg := NewToolRegistry()
	var wg sync.WaitGroup

	for i := range 10 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			tool := &ToolFunc{
				Def: ToolDef{Name: "tool"},
				Fn:  func(ctx context.Context, args map[string]any) (string, error) { return "", nil },
			}
			reg.Register(tool)
			reg.Get("tool")
			reg.Definitions()
		}(i)
	}
	wg.Wait()
}

// unwrapTool decorates a tool and exposes it through Unwrap.
type unwrapTool struct{ Tool }

func (u unwrapTool) Unwrap() Tool { return u.Tool }

func TestAs(t *testing.T) {
	base := &ToolFunc{Def: ToolDef{Name: "base"}}
	marked := WithMarkers(base, Marker{Kind: "approval"})
	tests := []struct {
		name string
		tool Tool
		want bool
	}{
		{"direct", marked, true},
		{"through one decorator", unwrapTool{marked}, true},
		{"through two decorators", unwrapTool{unwrapTool{marked}}, true},
		{"absent", unwrapTool{base}, false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := As[*MarkedTool](tt.tool)
			if ok != tt.want || (ok && got != marked) {
				t.Fatalf("As = %v, %v; want %v", got, ok, tt.want)
			}
		})
	}
}

func TestRegistryUniqueAndUnregister(t *testing.T) {
	r := NewToolRegistry()
	a := &ToolFunc{Def: ToolDef{Name: "a"}}
	if err := r.RegisterUnique(a); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterUnique(&ToolFunc{Def: ToolDef{Name: "a"}}); !errors.Is(err, ErrToolExists) {
		t.Fatalf("duplicate = %v, want ErrToolExists", err)
	}
	if got, _ := r.Get("a"); got != Tool(a) {
		t.Fatal("a duplicate replaced the registered tool")
	}
	if !r.Unregister("a") || r.Unregister("a") {
		t.Fatal("Unregister must report presence once")
	}
	if _, ok := r.Get("a"); ok {
		t.Fatal("tool still registered")
	}
}

func TestZeroToolRegistryIsUsable(t *testing.T) {
	var r ToolRegistry
	if _, ok := r.Get("x"); ok || len(r.All()) != 0 {
		t.Fatal("zero registry is not empty")
	}
	r.Register(&ToolFunc{Def: ToolDef{Name: "a"}})
	var u ToolRegistry
	if err := u.RegisterUnique(&ToolFunc{Def: ToolDef{Name: "b"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Get("a"); !ok {
		t.Fatal("Register on a zero registry lost the tool")
	}
	if _, ok := u.Get("b"); !ok {
		t.Fatal("RegisterUnique on a zero registry lost the tool")
	}
}
