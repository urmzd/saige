package openai

import (
	"github.com/openai/openai-go/v3"
	"testing"
)

func TestPromptCacheOptions(t *testing.T) {
	a := NewAdapter("unused", "gpt-4.1-mini", WithPromptCache("tenant-profile", "in-memory"))
	var params openai.ChatCompletionNewParams
	if err := a.applyPromptCache(&params); err != nil {
		t.Fatal(err)
	}
	if params.PromptCacheKey.Value != "tenant-profile" || params.PromptCacheRetention != "in_memory" {
		t.Fatal("cache options lost")
	}
	a = NewAdapter("unused", "gpt-4.1-mini", WithPromptCache("scope", "forever"))
	if err := a.applyPromptCache(&params); err == nil {
		t.Fatal("invalid retention accepted")
	}
}
