package eval

import (
	"math"
	"testing"
)

func assertClose(t *testing.T, name string, got, want, eps float64) {
	t.Helper()
	if math.Abs(got-want) > eps {
		t.Errorf("%s: got %f, want %f (±%f)", name, got, want, eps)
	}
}

func TestSequenceSimilarityIdentical(t *testing.T) {
	assertClose(t, "identical", SequenceSimilarity("hello world", "hello world"), 1.0, 0.001)
}

func TestSequenceSimilarityEmpty(t *testing.T) {
	assertClose(t, "both empty", SequenceSimilarity("", ""), 1.0, 0.001)
	assertClose(t, "one empty", SequenceSimilarity("hello", ""), 0.0, 0.001)
	assertClose(t, "other empty", SequenceSimilarity("", "hello"), 0.0, 0.001)
}

func TestSequenceSimilarityPartial(t *testing.T) {
	score := SequenceSimilarity("abcdef", "abcxyz")
	if score <= 0.0 || score >= 1.0 {
		t.Errorf("expected partial similarity, got %f", score)
	}
}

func TestTokenF1Identical(t *testing.T) {
	assertClose(t, "identical", TokenF1("the quick brown fox", "the quick brown fox"), 1.0, 0.001)
}

func TestTokenF1Empty(t *testing.T) {
	assertClose(t, "both empty", TokenF1("", ""), 1.0, 0.001)
	assertClose(t, "one empty", TokenF1("hello", ""), 0.0, 0.001)
}

func TestTokenF1Partial(t *testing.T) {
	// "the fox" vs "the quick brown fox"
	// overlap = 2 (the, fox), precision = 2/4, recall = 2/2
	// F1 = 2 * 0.5 * 1.0 / 1.5 = 0.6667
	score := TokenF1("the fox", "the quick brown fox")
	assertClose(t, "partial", score, 2.0/3.0, 0.001)
}

func TestTokenF1CaseInsensitive(t *testing.T) {
	assertClose(t, "case", TokenF1("Hello World", "hello world"), 1.0, 0.001)
}

func TestRougeLIdentical(t *testing.T) {
	assertClose(t, "identical", RougeL("the quick brown fox", "the quick brown fox"), 1.0, 0.001)
}

func TestRougeLEmpty(t *testing.T) {
	assertClose(t, "both empty", RougeL("", ""), 1.0, 0.001)
	assertClose(t, "one empty", RougeL("hello", ""), 0.0, 0.001)
}

func TestRougeLPartial(t *testing.T) {
	// "the fox jumps" vs "the quick brown fox jumps over"
	// LCS = [the, fox, jumps] = 3
	// precision = 3/6, recall = 3/3 = 1.0
	// F1 = 2 * 0.5 * 1.0 / 1.5 = 0.6667
	score := RougeL("the fox jumps", "the quick brown fox jumps over")
	assertClose(t, "partial", score, 2.0/3.0, 0.001)
}

func TestComputeContentQuality(t *testing.T) {
	q := ComputeContentQuality("hello world", "hello world")
	assertClose(t, "seq_sim", q.SequenceSimilarity, 1.0, 0.001)
	assertClose(t, "token_f1", q.TokenF1, 1.0, 0.001)
	assertClose(t, "rouge_l", q.RougeL, 1.0, 0.001)
}

func TestTokenF1IgnoresPunctuationAndCase(t *testing.T) {
	assertClose(t, "punctuation", TokenF1("Paris.", "paris"), 1.0, 0.001)
	assertClose(t, "mixed", TokenF1("The answer is: Paris!", "the answer is paris"), 1.0, 0.001)
	assertClose(t, "only punctuation", TokenF1("...", "!!!"), 1.0, 0.001)
}

func TestRougeLIgnoresPunctuationAndCase(t *testing.T) {
	assertClose(t, "punctuation", RougeL("Hello, World!", "hello world"), 1.0, 0.001)
	assertClose(t, "comma-joined", RougeL("red,green,blue", "Red Green Blue"), 1.0, 0.001)
}
