package contextassembler_test

import (
	"context"
	"strings"
	"testing"

	"github.com/urmzd/saige/rag/contextassembler"
	"github.com/urmzd/saige/rag/tokenizer"
	"github.com/urmzd/saige/rag/types"
)

func TestDefaultAssemblerDropsRepeatedText(t *testing.T) {
	hit := func(uuid, text string) types.SearchHit {
		return types.SearchHit{Variant: types.ContentVariant{UUID: uuid, Text: text}}
	}
	tests := []struct {
		name      string
		hits      []types.SearchHit
		wantTexts []string
		wantCites []string
	}{
		{
			name:      "distinct texts",
			hits:      []types.SearchHit{hit("a", "alpha"), hit("b", "beta")},
			wantTexts: []string{"alpha", "beta"},
			wantCites: []string{"[1]", "[2]"},
		},
		{
			name:      "same section text from two variants",
			hits:      []types.SearchHit{hit("a", "Section body."), hit("b", " Section body.\n"), hit("c", "gamma")},
			wantTexts: []string{"Section body.", "gamma"},
			wantCites: []string{"[1]", "[2]"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := (&contextassembler.DefaultAssembler{}).Assemble(context.Background(), "q", tt.hits)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Blocks) != len(tt.wantTexts) {
				t.Fatalf("got %d blocks, want %d", len(got.Blocks), len(tt.wantTexts))
			}
			for i, b := range got.Blocks {
				if b.Text != tt.wantTexts[i] || b.Citation != tt.wantCites[i] {
					t.Errorf("block %d = %q %s, want %q %s", i, b.Text, b.Citation, tt.wantTexts[i], tt.wantCites[i])
				}
			}
		})
	}
}

func TestDefaultAssemblerSkipsOversizedHit(t *testing.T) {
	hits := []types.SearchHit{
		{Variant: types.ContentVariant{UUID: "big", Text: strings.Repeat("token ", 600)}},
		{Variant: types.ContentVariant{UUID: "small", Text: "a short passage"}},
	}
	a := &contextassembler.DefaultAssembler{MaxTokens: 500}
	result, err := a.Assemble(context.Background(), "q", hits)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Blocks) != 1 || result.Blocks[0].Text != "a short passage" {
		t.Fatalf("blocks = %+v, want only the small hit", result.Blocks)
	}
}

func TestDefaultAssemblerBudgetCoversWholePrompt(t *testing.T) {
	hits := []types.SearchHit{
		{Variant: types.ContentVariant{UUID: "a", Text: "alpha"}, Provenance: types.Provenance{SourceURI: "https://example.com/a/very/long/source/path"}},
		{Variant: types.ContentVariant{UUID: "b", Text: "beta"}, Provenance: types.Provenance{SourceURI: "https://example.com/another/long/source/path"}},
	}
	a := &contextassembler.DefaultAssembler{MaxTokens: 40}
	result, err := a.Assemble(context.Background(), "q", hits)
	if err != nil {
		t.Fatal(err)
	}
	if n := tokenizer.CountTokens(result.Prompt); n > 40 {
		t.Errorf("prompt is %d tokens, budget 40", n)
	}
	if len(result.Blocks) == 0 || result.TokenCount > 40 {
		t.Errorf("blocks = %d, TokenCount = %d; want at least one block within 40", len(result.Blocks), result.TokenCount)
	}
}
