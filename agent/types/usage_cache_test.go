package types

import "testing"

func TestUsageSeparatesProviderAndResponseCaches(t *testing.T) {
	delta := UsageDelta{PromptTokens: 100, CompletionTokens: 20, CachedPromptTokens: 60, CacheWriteTokens: 10}
	usage := UsageFromDelta(delta)
	if usage.InputTokens != 30 || usage.CachedInputTokens != 60 || usage.CacheWriteTokens != 10 || usage.Total() != 120 || usage.Requests != 1 {
		t.Fatalf("wrong billing tiers: %+v", usage)
	}
	delta.CacheHit = true
	if usage = UsageFromDelta(delta); usage.Total() != 0 || usage.Requests != 0 {
		t.Fatal("response replay billed")
	}
}
