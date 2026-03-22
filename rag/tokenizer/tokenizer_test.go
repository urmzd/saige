package tokenizer_test

import (
	"testing"

	"github.com/urmzd/graph-agent-dev-kit/rag/tokenizer"
)

func TestNewEncoding(t *testing.T) {
	tok, err := tokenizer.New("cl100k_base")
	if err != nil {
		t.Fatal(err)
	}

	count := tok.Count("hello world")
	if count != 2 {
		t.Errorf("expected 2 tokens for 'hello world', got %d", count)
	}
}

func TestCountTokens(t *testing.T) {
	count := tokenizer.CountTokens("The quick brown fox jumps over the lazy dog")
	// cl100k_base tokenizes this to ~9 tokens, not 44/4=11
	if count < 5 || count > 15 {
		t.Errorf("unexpected token count %d for standard sentence", count)
	}
}

func TestEmptyString(t *testing.T) {
	count := tokenizer.CountTokens("")
	if count != 0 {
		t.Errorf("expected 0 tokens for empty string, got %d", count)
	}
}

func TestDefault(t *testing.T) {
	tok := tokenizer.Default()
	if tok == nil {
		t.Fatal("default tokenizer should not be nil")
	}

	// Verify it returns consistent results.
	c1 := tok.Count("test")
	c2 := tok.Count("test")
	if c1 != c2 {
		t.Errorf("inconsistent counts: %d vs %d", c1, c2)
	}
}
