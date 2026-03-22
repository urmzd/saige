package hyde_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/urmzd/graph-agent-dev-kit/rag/hyde"
)

type mockLLM struct {
	callCount atomic.Int32
}

func (m *mockLLM) Generate(_ context.Context, prompt string) (string, error) {
	n := m.callCount.Add(1)
	return fmt.Sprintf("hypothetical answer %d", n), nil
}

func TestHyDETransformer(t *testing.T) {
	llm := &mockLLM{}
	transformer := hyde.New(hyde.Config{
		LLM:             llm,
		NumHypothetical: 3,
	})

	queries, err := transformer.Transform(context.Background(), "What is attention?")
	if err != nil {
		t.Fatal(err)
	}

	// Should return original query + 3 hypotheticals.
	if len(queries) != 4 {
		t.Fatalf("expected 4 queries, got %d", len(queries))
	}

	if queries[0] != "What is attention?" {
		t.Errorf("first query should be original, got %q", queries[0])
	}

	if llm.callCount.Load() != 3 {
		t.Errorf("expected 3 LLM calls, got %d", llm.callCount.Load())
	}

	// Each hypothetical should be different.
	seen := make(map[string]bool)
	for _, q := range queries[1:] {
		if seen[q] {
			t.Errorf("duplicate hypothetical: %q", q)
		}
		seen[q] = true
	}
}

func TestHyDEDefaults(t *testing.T) {
	llm := &mockLLM{}
	// Zero NumHypothetical defaults to 3.
	transformer := hyde.New(hyde.Config{LLM: llm})

	queries, err := transformer.Transform(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}

	if len(queries) != 4 {
		t.Fatalf("expected 4 queries with default num, got %d", len(queries))
	}
}
