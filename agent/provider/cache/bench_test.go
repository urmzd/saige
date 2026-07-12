package cache

import (
	"context"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/cache/memcache"
	"github.com/urmzd/saige/agent/types"
)

func benchMessages() []types.Message {
	return []types.Message{
		types.NewSystemMessage("You are a helpful assistant with a long, stable system prompt."),
		types.NewUserMessage("Summarize the key benefits of response caching in three bullet points."),
	}
}

// BenchmarkKey measures the deterministic cache-key computation: the cost paid
// on every request (hit or miss) to look up the cache.
func BenchmarkKey(b *testing.B) {
	msgs := benchMessages()
	tools := []types.ToolDef{{Name: "search", Description: "search"}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Key("gpt-4o-mini", msgs, tools, nil)
	}
}

// BenchmarkCacheHit measures replaying a recorded response from the cache: the
// hot path that avoids an upstream provider call entirely.
func BenchmarkCacheHit(b *testing.B) {
	inner := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		{types.TextStartDelta{}, types.TextContentDelta{Content: "cached answer"}, types.TextEndDelta{},
			types.UsageDelta{PromptTokens: 50, CompletionTokens: 12, TotalTokens: 62}},
	}}
	p := New(inner, Config{Cache: memcache.New[CachedResponse]()})
	msgs := benchMessages()

	// Prime the cache (miss) so every measured call is a hit.
	drain(mustCh(p.ChatStream(context.Background(), msgs, nil)))

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ch, _ := p.ChatStream(context.Background(), msgs, nil)
		drain(ch)
	}
}

// BenchmarkCacheMiss measures the record-and-tee overhead on a cache miss (the
// cost the decorator adds when it has to call upstream and record the stream).
func BenchmarkCacheMiss(b *testing.B) {
	msgs := benchMessages()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Fresh empty cache each iteration so every call is a miss.
		inner := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
			{types.TextStartDelta{}, types.TextContentDelta{Content: "fresh"}, types.TextEndDelta{}},
		}}
		p := New(inner, Config{Cache: memcache.New[CachedResponse]()})
		ch, _ := p.ChatStream(context.Background(), msgs, nil)
		drain(ch)
	}
}

func mustCh(ch <-chan types.Delta, _ error) <-chan types.Delta { return ch }

func drain(ch <-chan types.Delta) {
	for range ch {
	}
}
