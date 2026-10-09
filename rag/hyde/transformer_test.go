package hyde_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/urmzd/saige/rag/hyde"
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

// scriptedLLM returns replies in call order; an empty reply with failOn set
// returns an error instead.
type scriptedLLM struct {
	calls   atomic.Int32
	replies []string
	failOn  map[int]bool
}

func (m *scriptedLLM) Generate(_ context.Context, _ string) (string, error) {
	n := int(m.calls.Add(1)) - 1
	if m.failOn[n] {
		return "", errors.New("rate limited")
	}
	return m.replies[n%len(m.replies)], nil
}

func TestHyDEPartialFailures(t *testing.T) {
	tests := []struct {
		name    string
		llm     *scriptedLLM
		want    int
		wantErr bool
	}{
		{name: "one failure keeps the rest", llm: &scriptedLLM{replies: []string{"answer"}, failOn: map[int]bool{1: true}}, want: 3, wantErr: true},
		{name: "all fail keeps raw query", llm: &scriptedLLM{replies: []string{"x"}, failOn: map[int]bool{0: true, 1: true, 2: true}}, want: 1, wantErr: true},
		{name: "blank hypotheticals dropped", llm: &scriptedLLM{replies: []string{"  \n", "answer"}}, want: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := hyde.New(hyde.Config{LLM: tt.llm, NumHypothetical: 3})
			queries, err := tr.Transform(context.Background(), "q")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, want error %v", err, tt.wantErr)
			}
			if len(queries) != tt.want || queries[0] != "q" {
				t.Errorf("queries = %q, want %d starting with the original", queries, tt.want)
			}
			for _, q := range queries {
				if strings.TrimSpace(q) != q || q == "" {
					t.Errorf("query %q is not trimmed", q)
				}
			}
		})
	}
}

func TestHyDECompileRejectsBadTemplate(t *testing.T) {
	tests := []struct {
		name     string
		template string
		wantErr  bool
	}{
		{name: "valid", template: "Answer: {{.Query}}"},
		{name: "unparsable", template: "{{.Query", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := hyde.Compile(hyde.Config{LLM: &mockLLM{}, PromptTemplate: tt.template})
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, want error %v", err, tt.wantErr)
			}
		})
	}
}

func TestHyDERenderErrorDoesNotPanic(t *testing.T) {
	// Calling a method on a string fails at execution time, not parse time.
	tr, err := hyde.Compile(hyde.Config{LLM: &mockLLM{}, PromptTemplate: "{{.Query.Missing}}"})
	if err != nil {
		t.Fatal(err)
	}
	queries, err := tr.Transform(context.Background(), "q")
	if err == nil {
		t.Fatal("expected render error")
	}
	if len(queries) != 1 || queries[0] != "q" {
		t.Errorf("queries = %q, want only the original", queries)
	}
}
