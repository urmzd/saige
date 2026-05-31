package cache

import (
	"context"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/cache/memcache"
	"github.com/urmzd/saige/agent/types"
)

func newProvider(inner types.Provider) *Provider {
	return New(inner, Config{Cache: memcache.New[CachedResponse]()})
}

func collect(ch <-chan types.Delta) []types.Delta {
	var out []types.Delta
	for d := range ch {
		out = append(out, d)
	}
	return out
}

func text(deltas []types.Delta) string {
	var s string
	for _, d := range deltas {
		if tc, ok := d.(types.TextContentDelta); ok {
			s += tc.Content
		}
	}
	return s
}

func TestMissThenHitText(t *testing.T) {
	inner := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.TextResponse("hello"),
	}}
	p := newProvider(inner)
	msgs := []types.Message{types.NewUserMessage("hi")}

	ch1, _ := p.ChatStream(context.Background(), msgs, nil)
	d1 := collect(ch1)
	if text(d1) != "hello" {
		t.Fatalf("miss text = %q, want hello", text(d1))
	}

	// Second identical call must be served from cache (inner has only 1 response).
	ch2, _ := p.ChatStream(context.Background(), msgs, nil)
	d2 := collect(ch2)
	if text(d2) != "hello" {
		t.Fatalf("hit text = %q, want hello", text(d2))
	}
}

func TestHitReportsCacheHitUsage(t *testing.T) {
	inner := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		{
			types.TextStartDelta{},
			types.TextContentDelta{Content: "x"},
			types.TextEndDelta{},
			types.UsageDelta{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		},
	}}
	p := newProvider(inner)
	msgs := []types.Message{types.NewUserMessage("q")}

	collect(mustStream(t, p, msgs)) // prime cache (miss)
	hit := collect(mustStream(t, p, msgs))

	var usage *types.UsageDelta
	for _, d := range hit {
		if u, ok := d.(types.UsageDelta); ok {
			uu := u
			usage = &uu
		}
	}
	if usage == nil {
		t.Fatal("expected a UsageDelta on cache hit")
	}
	if !usage.CacheHit {
		t.Error("expected UsageDelta.CacheHit == true on hit")
	}
	if usage.PromptTokens != 10 || usage.CompletionTokens != 5 {
		t.Errorf("expected original token counts preserved, got %+v", usage)
	}
}

func TestDistinctMessagesDistinctKeys(t *testing.T) {
	inner := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.TextResponse("a"),
		agenttest.TextResponse("b"),
	}}
	p := newProvider(inner)

	collect(mustStream(t, p, []types.Message{types.NewUserMessage("first")}))
	d := collect(mustStream(t, p, []types.Message{types.NewUserMessage("second")}))
	if text(d) != "b" {
		t.Fatalf("second distinct call text = %q, want b (a fresh miss)", text(d))
	}
}

func TestToolCallIsCached(t *testing.T) {
	inner := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("t1", "search", map[string]any{"q": "go"}),
	}}
	p := newProvider(inner)
	msgs := []types.Message{types.NewUserMessage("find")}

	collect(mustStream(t, p, msgs)) // miss records the tool-call deltas
	calls := agenttest.CollectToolCalls(replayChan(collect(mustStream(t, p, msgs))))
	if len(calls) != 1 || calls[0].Name != "search" {
		t.Fatalf("expected cached tool call 'search', got %+v", calls)
	}
}

func TestErrorStreamNotCached(t *testing.T) {
	inner := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		{types.TextStartDelta{}, types.ErrorDelta{Error: context.DeadlineExceeded}},
		agenttest.TextResponse("recovered"),
	}}
	p := newProvider(inner)
	msgs := []types.Message{types.NewUserMessage("q")}

	collect(mustStream(t, p, msgs)) // first call errors → must NOT cache
	d := collect(mustStream(t, p, msgs))
	if text(d) != "recovered" {
		t.Fatalf("second call text = %q, want recovered (proving error not cached)", text(d))
	}
}

func TestFileBytesAffectKey(t *testing.T) {
	mk := func(data []byte) []types.Message {
		return []types.Message{types.UserMessage{Content: []types.UserContent{
			types.FileContent{URI: "mem://x", MediaType: types.MediaPNG, Data: data},
		}}}
	}
	k1 := Key("m", mk([]byte("AAAA")), nil, nil)
	k2 := Key("m", mk([]byte("BBBB")), nil, nil)
	if k1 == k2 {
		t.Fatal("different file bytes must produce different keys")
	}
	k3 := Key("m", mk([]byte("AAAA")), nil, nil)
	if k1 != k3 {
		t.Fatal("identical file bytes must produce identical keys")
	}
}

func TestArgMapOrderStable(t *testing.T) {
	// Two assistant tool-use messages with maps built in different insertion
	// order but equal content must hash identically.
	a := []types.Message{types.AssistantMessage{Content: []types.AssistantContent{
		types.ToolUseContent{ID: "1", Name: "f", Arguments: map[string]any{"a": 1.0, "b": 2.0}},
	}}}
	b := []types.Message{types.AssistantMessage{Content: []types.AssistantContent{
		types.ToolUseContent{ID: "1", Name: "f", Arguments: map[string]any{"b": 2.0, "a": 1.0}},
	}}}
	if Key("m", a, nil, nil) != Key("m", b, nil, nil) {
		t.Fatal("argument map ordering must not affect the cache key")
	}
}

func TestSchemaAndToolsAffectKey(t *testing.T) {
	msgs := []types.Message{types.NewUserMessage("q")}
	base := Key("m", msgs, nil, nil)
	withSchema := Key("m", msgs, nil, &types.ParameterSchema{Type: "object"})
	if base == withSchema {
		t.Error("schema must affect the key")
	}
	withTools := Key("m", msgs, []types.ToolDef{{Name: "t"}}, nil)
	if base == withTools {
		t.Error("tools must affect the key")
	}
}

func TestNameDecorates(t *testing.T) {
	p := newProvider(&agenttest.ScriptedProvider{})
	if got := p.Name(); got != "cache(unknown)" {
		t.Errorf("Name() = %q, want cache(unknown)", got)
	}
}

func mustStream(t *testing.T, p *Provider, msgs []types.Message) <-chan types.Delta {
	t.Helper()
	ch, err := p.ChatStream(context.Background(), msgs, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	return ch
}

// replayChan re-feeds a delta slice through a channel for CollectToolCalls.
func replayChan(deltas []types.Delta) <-chan types.Delta {
	ch := make(chan types.Delta, len(deltas))
	for _, d := range deltas {
		ch <- d
	}
	close(ch)
	return ch
}
