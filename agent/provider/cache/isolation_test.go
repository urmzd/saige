package cache

import (
	"context"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/cache/memcache"
	"github.com/urmzd/saige/agent/types"
)

func TestPrivateNamespaceAndExplicitConfigurationScope(t *testing.T) {
	store := memcache.New[CachedResponse]()
	makeProvider := func(answer, scope, config string) *Provider {
		return New(&agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse(answer)}}, Config{Cache: store, ScopeKey: scope, ConfigKey: config})
	}
	msgs := []types.Message{types.NewUserMessage("same")}
	first := makeProvider("first", "", "")
	second := makeProvider("second", "", "")
	collect(mustStream(t, first, msgs))
	if got := text(collect(mustStream(t, second, msgs))); got != "second" {
		t.Fatal("private configurations collided", got)
	}
	first = makeProvider("shared", "tenant", "profile-v1")
	second = makeProvider("unused", "tenant", "profile-v1")
	collect(mustStream(t, first, msgs))
	if got := text(collect(mustStream(t, second, msgs))); got != "shared" {
		t.Fatal("explicit identity did not reuse", got)
	}
	third := makeProvider("other", "tenant", "profile-v2")
	if got := text(collect(mustStream(t, third, msgs))); got != "other" {
		t.Fatal("configuration revisions collided")
	}
}

func TestCachedToolArgumentsAndIDsAreIndependent(t *testing.T) {
	inner := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.ToolCallResponse("original", "read", map[string]any{"nested": map[string]any{"value": "clean"}})}}
	p := New(inner, Config{Cache: memcache.New[CachedResponse](), CacheToolCalls: true})
	msgs := []types.Message{types.NewUserMessage("read")}
	first := agenttest.CollectToolCalls(mustStream(t, p, msgs))
	first[0].Arguments["nested"].(map[string]any)["value"] = "corrupt"
	second := agenttest.CollectToolCalls(mustStream(t, p, msgs))
	if second[0].ID == first[0].ID || second[0].Arguments["nested"].(map[string]any)["value"] != "clean" {
		t.Fatal("cached arguments or call ID reused")
	}
	second[0].Arguments["nested"].(map[string]any)["value"] = "changed"
	third := agenttest.CollectToolCalls(mustStream(t, p, msgs))
	if third[0].ID == second[0].ID || third[0].Arguments["nested"].(map[string]any)["value"] != "clean" {
		t.Fatal("replay mutated store")
	}
}

func TestIncompleteResponseNotCachedAndCitationsRetained(t *testing.T) {
	inner := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		{types.TextStartDelta{}, types.TextContentDelta{Content: "partial"}},
		append(agenttest.TextResponse("complete"), types.CitationDelta{Citation: types.Citation{URI: "https://example.com"}}),
	}}
	p := newProvider(inner)
	msgs := []types.Message{types.NewUserMessage("task")}
	collect(mustStream(t, p, msgs))
	collect(mustStream(t, p, msgs))
	got := collect(mustStream(t, p, msgs))
	if text(got) != "complete" {
		t.Fatal("partial cached")
	}
	found := false
	for _, d := range got {
		if _, ok := d.(types.CitationDelta); ok {
			found = true
		}
	}
	if !found {
		t.Fatal("citation lost on replay")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.ChatStream(ctx, msgs, nil); err == nil {
		t.Fatal("cancelled cache read succeeded")
	}
}

func TestToolOrderIsPartOfResponseIdentity(t *testing.T) {
	a := []types.ToolDef{{Name: "first"}, {Name: "second"}}
	b := []types.ToolDef{a[1], a[0]}
	if Key("model", nil, a, nil) == Key("model", nil, b, nil) {
		t.Fatal("different provider prompts share a key")
	}
}
