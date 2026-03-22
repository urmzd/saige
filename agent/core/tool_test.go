package core

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
