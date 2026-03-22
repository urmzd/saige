// Package tokenizer provides accurate token counting for RAG pipelines.
package tokenizer

import (
	"sync"

	"github.com/pkoukk/tiktoken-go"
)

// Tokenizer counts tokens using tiktoken encoding.
type Tokenizer struct {
	enc *tiktoken.Tiktoken
}

var (
	defaultTokenizer *Tokenizer
	defaultOnce      sync.Once
)

// New creates a Tokenizer for the given encoding name (e.g., "cl100k_base", "o200k_base").
func New(encoding string) (*Tokenizer, error) {
	enc, err := tiktoken.GetEncoding(encoding)
	if err != nil {
		return nil, err
	}
	return &Tokenizer{enc: enc}, nil
}

// NewForModel creates a Tokenizer for a specific model name (e.g., "gpt-4", "gpt-4o").
func NewForModel(model string) (*Tokenizer, error) {
	enc, err := tiktoken.EncodingForModel(model)
	if err != nil {
		return nil, err
	}
	return &Tokenizer{enc: enc}, nil
}

// Default returns a shared Tokenizer using cl100k_base encoding.
// Falls back to len(text)/4 estimation if tiktoken initialization fails.
func Default() *Tokenizer {
	defaultOnce.Do(func() {
		t, err := New("cl100k_base")
		if err == nil {
			defaultTokenizer = t
		}
	})
	return defaultTokenizer
}

// Count returns the number of tokens in the text.
func (t *Tokenizer) Count(text string) int {
	return len(t.enc.Encode(text, nil, nil))
}

// CountTokens counts tokens using the default tokenizer.
// Falls back to len(text)/4 if no tokenizer is available.
func CountTokens(text string) int {
	if t := Default(); t != nil {
		return t.Count(text)
	}
	return len(text) / 4
}
