package toolcache

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/types"
)

func cachedPolicy() types.CachePolicy {
	return types.CachePolicy{Enabled: true, TTL: time.Minute, Scope: types.CacheScopeGlobal}
}

// The agent loop finds markers with a `tool.(*types.MarkedTool)` type assertion.
// Any decorator in front of a MarkedTool defeats that assertion, and the human
// approval prompt is then skipped without a word. The cache therefore has to go
// inside the markers, never around them.

func TestWrappingAMarkedToolDirectlyIsRefused(t *testing.T) {
	marked := types.WithMarkers(
		&countingTool{result: "x", policy: cachedPolicy()},
		types.Marker{Kind: "destructive", Message: "really?"},
	)

	_, err := New(marked, Config{Cache: newMemCache()})
	if err == nil {
		t.Fatal("caching a marked tool was accepted; it would hide the approval prompt")
	}
	if !strings.Contains(err.Error(), "INSIDE") {
		t.Errorf("error does not say how to compose it correctly: %v", err)
	}
}

func TestWrapAllKeepsMarkersOutsideTheCache(t *testing.T) {
	inner := &countingTool{result: "x", policy: cachedPolicy()}
	marker := types.Marker{Kind: "destructive", Message: "really?"}

	src := types.NewToolRegistry()
	src.Register(types.WithMarkers(inner, marker))

	out, err := WrapAll(src, Config{Cache: newMemCache()})
	if err != nil {
		t.Fatal(err)
	}

	got, found := out.Get("search")
	if !found {
		t.Fatal("tool missing from the wrapped registry")
	}

	// Still a MarkedTool, so the loop still prompts.
	mt, ok := got.(*types.MarkedTool)
	if !ok {
		t.Fatalf("got %T, want *types.MarkedTool: the approval prompt is skipped otherwise", got)
	}
	if len(mt.Markers) != 1 || mt.Markers[0].Kind != "destructive" {
		t.Fatalf("markers lost in the rewrap: %+v", mt.Markers)
	}
	// ...and the work underneath is still memoized.
	if _, isCache := mt.Inner.(*Tool); !isCache {
		t.Fatalf("inner is %T, want the cache decorator", mt.Inner)
	}

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := mt.Execute(ctx, map[string]any{"q": "a"}); err != nil {
			t.Fatal(err)
		}
	}
	if inner.count() != 1 {
		t.Errorf("inner ran %d times, want 1: the cache under the markers is not working", inner.count())
	}
}
