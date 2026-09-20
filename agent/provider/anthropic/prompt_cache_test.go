package anthropic

import (
	"github.com/anthropics/anthropic-sdk-go"
	"testing"
)

func TestPromptCacheBoundary(t *testing.T) {
	a := NewAdapter("unused", "claude-sonnet-4-5", WithSystemPromptCache("1h"))
	blocks := []anthropic.TextBlockParam{{Text: "first"}, {Text: "last"}}
	if err := a.applyPromptCache(blocks); err != nil {
		t.Fatal(err)
	}
	if blocks[1].CacheControl.TTL != "1h" || blocks[0].CacheControl.TTL != "" {
		t.Fatal("wrong cache boundary")
	}
	if err := a.applyPromptCache(nil); err == nil {
		t.Fatal("missing prefix accepted")
	}
}
