package cache

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/urmzd/saige/agent/cache/memcache"
	"github.com/urmzd/saige/agent/types"
)

// optionsProvider counts upstream calls and answers every request with text.
type optionsProvider struct{ calls atomic.Int32 }

func (p *optionsProvider) ChatStream(context.Context, []types.Message, []types.ToolDef) (<-chan types.Delta, error) {
	p.calls.Add(1)
	ch := make(chan types.Delta, 3)
	ch <- types.TextStartDelta{}
	ch <- types.TextContentDelta{Content: "answer"}
	ch <- types.TextEndDelta{}
	close(ch)
	return ch, nil
}

func (p *optionsProvider) ChatStreamWithOptions(ctx context.Context, m []types.Message, tools []types.ToolDef, _ types.RequestOptions) (<-chan types.Delta, error) {
	return p.ChatStream(ctx, m, tools)
}

func TestRequestOptionsArePartOfTheKey(t *testing.T) {
	none := types.ToolChoice{Mode: types.ToolChoiceNone}
	auto := types.ToolChoice{Mode: types.ToolChoiceAuto}
	inner := &optionsProvider{}
	p := New(inner, Config{Cache: memcache.New[CachedResponse]()})
	msgs := []types.Message{types.NewUserMessage("q")}
	drainAll := func(ch <-chan types.Delta, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		for range ch {
		}
	}
	for _, tc := range []struct {
		name      string
		call      func() (<-chan types.Delta, error)
		wantCalls int32
	}{
		{"plain request misses", func() (<-chan types.Delta, error) { return p.ChatStream(context.Background(), msgs, nil) }, 1},
		{"plain request hits", func() (<-chan types.Delta, error) { return p.ChatStream(context.Background(), msgs, nil) }, 1},
		{"options miss the plain entry", func() (<-chan types.Delta, error) {
			return p.ChatStreamWithOptions(context.Background(), msgs, nil, types.RequestOptions{ToolChoice: &none})
		}, 2},
		{"same options hit", func() (<-chan types.Delta, error) {
			return p.ChatStreamWithOptions(context.Background(), msgs, nil, types.RequestOptions{ToolChoice: &none})
		}, 2},
		{"different options miss", func() (<-chan types.Delta, error) {
			return p.ChatStreamWithOptions(context.Background(), msgs, nil, types.RequestOptions{ToolChoice: &auto})
		}, 3},
	} {
		drainAll(tc.call())
		if got := inner.calls.Load(); got != tc.wantCalls {
			t.Fatalf("%s: upstream calls = %d, want %d", tc.name, got, tc.wantCalls)
		}
	}
}
