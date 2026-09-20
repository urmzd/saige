package google

import (
	"testing"
	"time"

	"github.com/urmzd/saige/agent/types"
)

func TestContextCacheBindsExactPrefixAndTools(t *testing.T) {
	a := &Adapter{model: "gemini-2.5-flash"}
	prefix := []types.Message{types.NewSystemMessage("rules"), types.NewUserMessage("reference")}
	tools := []types.ToolDef{{Name: "read", Parameters: types.ParameterSchema{Type: "object"}}}
	contents, config := a.buildRequest(prefix, tools)
	fingerprint, err := cacheFingerprint(contents, config)
	if err != nil {
		t.Fatal(err)
	}
	cache := ContextCache{Name: "cachedContents/test", Model: a.model, PrefixCount: 2, Fingerprint: fingerprint, ExpiresAt: time.Now().Add(time.Hour)}
	a.contextCache = &cache
	request := append(append([]types.Message(nil), prefix...), types.NewUserMessage("question"))
	remainder, bound, err := a.cachedRequest(request, tools)
	if err != nil || len(remainder) != 1 || bound.CachedContent != cache.Name || bound.SystemInstruction != nil || len(bound.Tools) != 0 {
		t.Fatal("cache prefix duplicated or binding failed", err)
	}
	if _, _, err = a.cachedRequest(request, nil); err == nil {
		t.Fatal("changed tools accepted")
	}
	changed := append([]types.Message(nil), request...)
	changed[0] = types.NewSystemMessage("new rules")
	if _, _, err = a.cachedRequest(changed, tools); err == nil {
		t.Fatal("changed prefix accepted")
	}
	other := *a
	other.model = "other"
	if _, _, err = other.cachedRequest(request, tools); err == nil {
		t.Fatal("cross-model cache accepted")
	}
	cache.ExpiresAt = time.Now().Add(-time.Hour)
	if _, _, err = a.cachedRequest(request, tools); err == nil {
		t.Fatal("expired cache accepted")
	}
}
