package toolcache

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/cache/memcache"
	"github.com/urmzd/saige/agent/types"
)

type rich struct{ result types.ToolResult }

func (r *rich) Definition() types.ToolDef                               { return types.ToolDef{Name: "rich"} }
func (r *rich) Execute(context.Context, map[string]any) (string, error) { return r.result.Text(), nil }
func (r *rich) ExecuteRich(context.Context, map[string]any) (types.ToolResult, error) {
	return r.result, nil
}
func TestResultCopiesAtEveryBoundary(t *testing.T) {
	inner := &rich{result: types.ToolResult{Parts: []types.ToolOutputPart{types.Image(types.Bytes(types.MediaPNG, []byte{1})), types.JSONPart{JSON: json.RawMessage(`{"x":1}`)}}, Citations: []types.Citation{{Meta: map[string]any{"nested": map[string]any{"value": "original"}}}}}}
	policy := types.CachePolicy{Enabled: true, TTL: time.Hour, Scope: types.CacheScopeGlobal}
	wrapped, err := New(inner, Config{Cache: newMemCache(), Policy: &policy})
	if err != nil {
		t.Fatal(err)
	}
	tool := wrapped.(types.RichTool)
	first, err := tool.ExecuteRich(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	first.Parts[0].(types.ImagePart).Source.Inline[0] = 9
	first.Parts[1].(types.JSONPart).JSON[0] = '!'
	first.Citations[0].Meta["nested"].(map[string]any)["value"] = "changed"
	inner.result.Parts[0].(types.ImagePart).Source.Inline[0] = 7
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			value, err := tool.ExecuteRich(context.Background(), nil)
			if err != nil {
				t.Error(err)
				return
			}
			if value.Parts[0].(types.ImagePart).Source.Inline[0] != 1 || value.Parts[1].(types.JSONPart).JSON[0] != '{' || value.Citations[0].Meta["nested"].(map[string]any)["value"] != "original" {
				t.Error("shared cached payload")
			}
			value.Parts[0].(types.ImagePart).Source.Inline[0] = 8
		}()
	}
	wg.Wait()
}
func TestStaleRetentionWithRealTTLStore(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	cache := memcache.New[Entry](memcache.WithClock[Entry](clock))
	inner := &countingTool{result: "old", policy: types.CachePolicy{Enabled: true, TTL: time.Second, Scope: types.CacheScopeGlobal, ServeStaleOnError: true, MaxStale: time.Minute}}
	tool, err := New(inner, Config{Cache: cache, Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	inner.err = errors.New("offline")
	if out, err := tool.Execute(context.Background(), nil); err != nil || out != "old" {
		t.Fatalf("%q %v", out, err)
	}
	now = now.Add(time.Minute)
	if _, err := tool.Execute(context.Background(), nil); err == nil {
		t.Fatal("served beyond max stale")
	}
}
func TestPrivateIdentityAndInvalidKey(t *testing.T) {
	cache := newMemCache()
	policy := types.CachePolicy{Enabled: true, TTL: time.Hour, Scope: types.CacheScopeGlobal}
	first := &countingTool{result: "one", policy: policy}
	second := &countingTool{result: "two", policy: policy}
	a, _ := New(first, Config{Cache: cache})
	b, _ := New(second, Config{Cache: cache})
	_, _ = a.Execute(context.Background(), nil)
	if result, err := b.Execute(context.Background(), nil); err != nil || result != "two" {
		t.Fatalf("%q %v", result, err)
	}
	if _, err := a.Execute(context.Background(), map[string]any{"value": make(chan int)}); err == nil {
		t.Fatal("accepted invalid identity")
	}
	if first.count() != 1 {
		t.Fatal("executed invalid call")
	}
}
