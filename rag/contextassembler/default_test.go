package contextassembler_test

import (
	"context"
	"testing"

	"github.com/urmzd/saige/rag/contextassembler"
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
