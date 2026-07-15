package chunker

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/urmzd/saige/rag/types"
)

func TestMakeTestSectionExactSizes(t *testing.T) {
	for _, size := range []int{0, 1, 44, 45, 46, 500, 5000, 50000} {
		t.Run(fmt.Sprintf("size=%d", size), func(t *testing.T) {
			sec := makeTestSection(size)
			if len(sec.Variants) != 1 {
				t.Fatalf("expected 1 variant, got %d", len(sec.Variants))
			}
			if got := len(sec.Variants[0].Text); got != size {
				t.Fatalf("expected text length %d, got %d", size, got)
			}
		})
	}
}

func TestRecursiveChunkerNormalizesUnsafeConfig(t *testing.T) {
	doc := makeEdgeDoc("short text")
	c := NewRecursive(&Config{MaxTokens: 0, Overlap: -10})

	result, err := c.Chunk(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Sections) != 1 {
		t.Fatalf("expected unsafe config to fall back to a single default-sized chunk, got %d", len(result.Sections))
	}
}

func TestRecursiveChunkerHardSplitWithoutSeparators(t *testing.T) {
	text := strings.Repeat("abcdefghijklmnopqrstuvwxyz ", 30)
	c := NewRecursive(&Config{MaxTokens: 5, Separators: []string{}})

	result, err := c.Chunk(context.Background(), makeEdgeDoc(text))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Sections) < 2 {
		t.Fatalf("expected hard split to produce multiple sections, got %d", len(result.Sections))
	}
	for i, sec := range result.Sections {
		got := estimateTokens(sec.Variants[0].Text)
		if got > 5 {
			t.Fatalf("section %d has %d tokens, want <= 5", i, got)
		}
	}
}

func TestRecursiveChunkerHardSplitKeepsValidUTF8(t *testing.T) {
	text := strings.Repeat("alpha🙂beta ", 40)
	c := NewRecursive(&Config{MaxTokens: 3, Separators: []string{}})

	result, err := c.Chunk(context.Background(), makeEdgeDoc(text))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Sections) < 2 {
		t.Fatalf("expected unicode text to be split, got %d section(s)", len(result.Sections))
	}
	for i, sec := range result.Sections {
		got := sec.Variants[0].Text
		if !utf8.ValidString(got) {
			t.Fatalf("section %d contains invalid UTF-8: %q", i, got)
		}
	}
}

func makeEdgeDoc(text string) *types.Document {
	return &types.Document{
		UUID: "doc-edge",
		Sections: []types.Section{{
			UUID:         "sec-edge",
			DocumentUUID: "doc-edge",
			Variants: []types.ContentVariant{{
				UUID:        "var-edge",
				SectionUUID: "sec-edge",
				ContentType: types.ContentText,
				MIMEType:    "text/plain",
				Text:        text,
			}},
		}},
	}
}
