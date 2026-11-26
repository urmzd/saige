package chunker

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/urmzd/saige/rag/types"
)

// topicEmbedder maps text containing "alpha" to one axis and everything else
// to an orthogonal axis.
type topicEmbedder struct{}

func (topicEmbedder) Register(types.ContentType, types.VariantEmbedder) {}

func (topicEmbedder) Embed(_ context.Context, vs []types.ContentVariant) ([][]float32, error) {
	out := make([][]float32, len(vs))
	for i, v := range vs {
		if strings.Contains(v.Text, "alpha") {
			out[i] = []float32{1, 0}
		} else {
			out[i] = []float32{0, 1}
		}
	}
	return out, nil
}

func sectionTexts(doc *types.Document) []string {
	var out []string
	for _, s := range doc.Sections {
		for _, v := range s.Variants {
			out = append(out, v.Text)
		}
	}
	return out
}

// Text without sentence punctuation must still respect MaxTokens.
func TestSemanticChunkerEnforcesMaxTokensWithoutPunctuation(t *testing.T) {
	c := NewSemantic(topicEmbedder{}, &SemanticConfig{MinTokens: 10, MaxTokens: 50})
	for name, text := range map[string]string{
		"words": strings.Repeat("word ", 2000),
		"code":  strings.Repeat("x := compute(y)\nreturn x\n", 300),
		"cjk":   strings.Repeat("这是一个测试句子没有西文标点", 200),
	} {
		t.Run(name, func(t *testing.T) {
			out, err := c.Chunk(context.Background(), makeEdgeDoc(text))
			if err != nil {
				t.Fatal(err)
			}
			texts := sectionTexts(out)
			if len(texts) < 2 {
				t.Fatalf("got %d chunks, want several", len(texts))
			}
			for i, s := range texts {
				if n := estimateTokens(s); n > 50 {
					t.Fatalf("chunk %d has %d tokens, want <= 50", i, n)
				}
				if !utf8.ValidString(s) {
					t.Fatalf("chunk %d is not valid UTF-8", i)
				}
			}
		})
	}
}

func TestSplitIntoSentencesCJKAndNewlines(t *testing.T) {
	got := splitIntoSentences("第一句。第二句！third line\nfourth line. fifth")
	want := []string{"第一句。", "第二句！", "third line", "fourth line.", "fifth"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// With fewer than 10 boundaries the auto threshold must still split on a
// clear topic change.
func TestSemanticChunkerAutoThresholdSmallSample(t *testing.T) {
	c := NewSemantic(topicEmbedder{}, &SemanticConfig{MinTokens: 1, MaxTokens: 500})
	text := "The alpha one is here. The alpha two is here. The beta one is here. The beta two is here. The beta three is here."
	out, err := c.Chunk(context.Background(), makeEdgeDoc(text))
	if err != nil {
		t.Fatal(err)
	}
	texts := sectionTexts(out)
	if len(texts) != 2 || !strings.Contains(texts[0], "alpha two") || strings.Contains(texts[1], "alpha") {
		t.Fatalf("want split at the topic change, got %q", texts)
	}
}

func TestSemanticChunkerAutoThresholdUniformDoesNotSplit(t *testing.T) {
	c := NewSemantic(topicEmbedder{}, &SemanticConfig{MinTokens: 1, MaxTokens: 500})
	text := "One beta here. Two beta here. Three beta here. Four beta here."
	out, err := c.Chunk(context.Background(), makeEdgeDoc(text))
	if err != nil {
		t.Fatal(err)
	}
	if texts := sectionTexts(out); len(texts) != 1 {
		t.Fatalf("uniform similarity should not split, got %q", texts)
	}
}

func TestPercentileInterpolates(t *testing.T) {
	if got := percentile([]float64{0, 1, 1, 1}, 10); got <= 0 || got >= 1 {
		t.Fatalf("percentile = %v, want strictly between min and next value", got)
	}
	if got := percentile([]float64{5}, 10); got != 5 {
		t.Fatalf("percentile of one value = %v, want 5", got)
	}
}

// Hard splitting long text without spaces must tokenize each byte a bounded
// number of times, not rescan the remaining text for every chunk.
func TestRecursiveChunkerHardSplitLargeNoSpaceText(t *testing.T) {
	b := make([]byte, 200_000)
	for i := range b {
		b[i] = "abcdefghijklmnopqrstuvwxyz0123456789+/"[(i*7919)%38]
	}
	tokenized := 0
	orig := countTokens
	countTokens = func(s string) int {
		tokenized += len(s)
		return orig(s)
	}
	t.Cleanup(func() { countTokens = orig })

	c := NewRecursive(&Config{MaxTokens: 512, Overlap: 50})
	out, err := c.Chunk(context.Background(), makeEdgeDoc(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("tokenized %d bytes for %d bytes of text", tokenized, len(b))
	if ratio := tokenized / len(b); ratio > 100 {
		t.Fatalf("tokenized %d bytes for a %d byte text (%dx), want at most 100x", tokenized, len(b), ratio)
	}
	for i, s := range sectionTexts(out) {
		if n := estimateTokens(s); n > 512 {
			t.Fatalf("chunk %d has %d tokens, want <= 512", i, n)
		}
	}
}

func TestHardSplitPreservesTextAndBudget(t *testing.T) {
	text := strings.Repeat("ab🙂", 500) + strings.Repeat("z", 3000)
	parts := hardSplit(text, 7)
	if strings.Join(parts, "") != text {
		t.Fatal("hard split lost or reordered text")
	}
	for i, p := range parts {
		if !utf8.ValidString(p) {
			t.Fatalf("part %d is not valid UTF-8", i)
		}
		if n := estimateTokens(p); n > 7 && utf8.RuneCountInString(p) > 1 {
			t.Fatalf("part %d has %d tokens, want <= 7", i, n)
		}
	}
	// A budget smaller than one rune still makes progress.
	if got := hardSplit("🙂🙂", 0); strings.Join(got, "") != "🙂🙂" {
		t.Fatalf("zero budget split = %q", got)
	}
}
