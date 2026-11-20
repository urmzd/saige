package contextassembler_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/urmzd/saige/rag/contextassembler"
	"github.com/urmzd/saige/rag/types"
)

type mockLLM struct{}

func (m *mockLLM) Generate(_ context.Context, prompt string) (string, error) {
	// Extract the text between "Text: " and "\n\nRelevant" and return first sentence.
	if idx := strings.Index(prompt, "Text: "); idx >= 0 {
		text := prompt[idx+6:]
		if end := strings.Index(text, "\n\n"); end >= 0 {
			text = text[:end]
		}
		// Return first sentence as "compressed".
		if dotIdx := strings.Index(text, "."); dotIdx >= 0 {
			return text[:dotIdx+1], nil
		}
		return text, nil
	}
	return "compressed", nil
}

func TestCompressingAssembler(t *testing.T) {
	hits := []types.SearchHit{
		{
			Variant: types.ContentVariant{
				UUID: "v1",
				Text: "First sentence. Second sentence. Third sentence.",
			},
			Provenance: types.Provenance{
				DocumentUUID: "d1",
				SourceURI:    "http://example.com/1",
			},
		},
		{
			Variant: types.ContentVariant{
				UUID: "v2",
				Text: "Another document. With more text.",
			},
			Provenance: types.Provenance{
				DocumentUUID: "d2",
				SourceURI:    "http://example.com/2",
			},
		},
	}

	a := contextassembler.NewCompressing(&mockLLM{}, 4096)
	result, err := a.Assemble(context.Background(), "test query", hits)
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(result.Blocks))
	}

	// Provenance should be preserved from original hits.
	if result.Blocks[0].Provenance.SourceURI != "http://example.com/1" {
		t.Errorf("expected original provenance URI, got %q", result.Blocks[0].Provenance.SourceURI)
	}
	if result.Blocks[1].Provenance.SourceURI != "http://example.com/2" {
		t.Errorf("expected original provenance URI, got %q", result.Blocks[1].Provenance.SourceURI)
	}

	// Citations should be numbered.
	if result.Blocks[0].Citation != "[1]" {
		t.Errorf("expected [1], got %q", result.Blocks[0].Citation)
	}
	if result.Blocks[1].Citation != "[2]" {
		t.Errorf("expected [2], got %q", result.Blocks[1].Citation)
	}
}

func TestCompressingAssemblerTokenLimit(t *testing.T) {
	// Create hits with very long text that exceeds token limit.
	longText := strings.Repeat("word ", 5000) // ~25000 chars
	hits := []types.SearchHit{
		{
			Variant:    types.ContentVariant{UUID: "v1", Text: longText},
			Provenance: types.Provenance{DocumentUUID: "d1"},
		},
		{
			Variant:    types.ContentVariant{UUID: "v2", Text: "short text"},
			Provenance: types.Provenance{DocumentUUID: "d2"},
		},
	}

	a := contextassembler.NewCompressing(&mockLLM{}, 100)
	result, err := a.Assemble(context.Background(), "test", hits)
	if err != nil {
		t.Fatal(err)
	}

	if result.TokenCount > 100 {
		t.Errorf("expected token count <= 100, got %d", result.TokenCount)
	}
}

type flakyLLM struct{}

func (flakyLLM) Generate(_ context.Context, prompt string) (string, error) {
	switch {
	case strings.Contains(prompt, "fails"):
		return "", errors.New("rate limited")
	case strings.Contains(prompt, "irrelevant"):
		return " n/a.\n", nil
	}
	return "kept", nil
}

func TestCompressingAssemblerDegradesPerHit(t *testing.T) {
	hits := []types.SearchHit{
		{Variant: types.ContentVariant{UUID: "1", Text: "this call fails"}},
		{Variant: types.ContentVariant{UUID: "2", Text: "irrelevant text"}},
		{Variant: types.ContentVariant{UUID: "3", Text: "useful text"}},
	}
	result, err := contextassembler.NewCompressing(flakyLLM{}, 4096).Assemble(context.Background(), "q", hits)
	if !errors.Is(err, types.ErrPartialSearch) {
		t.Fatalf("err = %v, want partial", err)
	}
	if result == nil || len(result.Blocks) != 2 {
		t.Fatalf("result = %+v, want 2 blocks", result)
	}
	if result.Blocks[0].Text != "this call fails" || result.Blocks[1].Text != "kept" {
		t.Errorf("blocks = %q, %q; want original text for the failed call, then the compressed one", result.Blocks[0].Text, result.Blocks[1].Text)
	}
}
