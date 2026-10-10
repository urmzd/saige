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

func (p *optionsProvider) chatStream(context.Context, []types.Message, []types.ToolDef) (<-chan types.Delta, error) {
	p.calls.Add(1)
	ch := make(chan types.Delta, 3)
	ch <- types.PartStart{Index: 0, Kind: types.KindText}
	ch <- types.PartDelta{Index: 0, Text: "answer"}
	ch <- types.PartEnd{Index: 0}
	close(ch)
	return ch, nil
}

// Stream implements types.Provider.
func (p *optionsProvider) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	if req.Options != nil {
		return p.chatStreamWithOptions(ctx, req.Messages, req.Tools, *req.Options)
	}
	return p.chatStream(ctx, req.Messages, req.Tools)
}

// SupportsOptions implements types.OptionsProvider.
func (p *optionsProvider) SupportsOptions() bool { return true }

func (p *optionsProvider) chatStreamWithOptions(ctx context.Context, m []types.Message, tools []types.ToolDef, _ types.RequestOptions) (<-chan types.Delta, error) {
	return p.Stream(ctx, types.Request{Messages: m, Tools: tools})
}

func TestRequestOptionsArePartOfTheKey(t *testing.T) {
	none := types.ToolChoice{Mode: types.ToolChoiceNone}
	auto := types.ToolChoice{Mode: types.ToolChoiceAuto}
	inner := &optionsProvider{}
	p := New(inner, Config{Cache: memcache.New[CachedResponse]()})
	msgs := []types.Message{types.UserMsg(types.Text("q"))}
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
		{"plain request misses", func() (<-chan types.Delta, error) {
			return p.Stream(context.Background(), types.Request{Messages: msgs})
		}, 1},
		{"plain request hits", func() (<-chan types.Delta, error) {
			return p.Stream(context.Background(), types.Request{Messages: msgs})
		}, 1},
		{"options miss the plain entry", func() (<-chan types.Delta, error) {
			return p.Stream(context.Background(), types.Request{Messages: msgs, Options: &types.RequestOptions{ToolChoice: &none}})
		}, 2},
		{"same options hit", func() (<-chan types.Delta, error) {
			return p.Stream(context.Background(), types.Request{Messages: msgs, Options: &types.RequestOptions{ToolChoice: &none}})
		}, 2},
		{"different options miss", func() (<-chan types.Delta, error) {
			return p.Stream(context.Background(), types.Request{Messages: msgs, Options: &types.RequestOptions{ToolChoice: &auto}})
		}, 3},
	} {
		drainAll(tc.call())
		if got := inner.calls.Load(); got != tc.wantCalls {
			t.Fatalf("%s: upstream calls = %d, want %d", tc.name, got, tc.wantCalls)
		}
	}
}
