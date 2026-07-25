package toolcache

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// memCache is a minimal types.Cache used to observe what the decorator stores.
type memCache struct {
	mu   sync.Mutex
	data map[string]Entry
}

func newMemCache() *memCache { return &memCache{data: map[string]Entry{}} }

func (m *memCache) Get(_ context.Context, key string) (Entry, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.data[key]
	return e, ok, nil
}
func (m *memCache) Set(_ context.Context, key string, v Entry, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = v
	return nil
}
func (m *memCache) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	return nil
}
func (m *memCache) Clear(context.Context) error { return nil }
func (m *memCache) len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.data)
}

// countingTool records how many times it actually ran.
type countingTool struct {
	mu     sync.Mutex
	calls  int
	policy types.CachePolicy
	result string
	err    error
	block  chan struct{}
}

func (c *countingTool) Definition() types.ToolDef { return types.ToolDef{Name: "search"} }
func (c *countingTool) CachePolicy() types.CachePolicy {
	return c.policy
}
func (c *countingTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	if c.block != nil {
		<-c.block
	}
	return c.result, c.err
}
func (c *countingTool) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func sessionScope() map[types.CacheScope]string {
	return map[types.CacheScope]string{types.CacheScopeSession: "s1"}
}

func TestUndeclaredPolicyLeavesTheToolUnwrapped(t *testing.T) {
	inner := &countingTool{result: "x"}
	got, err := New(inner, Config{Cache: newMemCache()})
	if err != nil {
		t.Fatal(err)
	}
	if got != types.Tool(inner) {
		t.Error("a tool with no cache policy must be returned unchanged, not silently cached")
	}
}

func TestRepeatedCallsHitTheCache(t *testing.T) {
	inner := &countingTool{
		result: "results",
		policy: types.CachePolicy{Enabled: true, TTL: time.Minute},
	}
	cached, err := New(inner, Config{Cache: newMemCache(), Scope: sessionScope()})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		out, err := cached.Execute(context.Background(), map[string]any{"q": "go"})
		if err != nil || out != "results" {
			t.Fatalf("call %d = %q, %v", i, out, err)
		}
	}
	if inner.count() != 1 {
		t.Errorf("tool ran %d times, want 1: the agent re-asks the same question and the cache exists to absorb that", inner.count())
	}
}

func TestDifferentArgumentsAreDifferentEntries(t *testing.T) {
	inner := &countingTool{result: "r", policy: types.CachePolicy{Enabled: true, TTL: time.Minute}}
	cached, _ := New(inner, Config{Cache: newMemCache(), Scope: sessionScope()})

	cached.Execute(context.Background(), map[string]any{"q": "a"})
	cached.Execute(context.Background(), map[string]any{"q": "b"})
	if inner.count() != 2 {
		t.Errorf("tool ran %d times, want 2", inner.count())
	}
}

// IgnoreArgs is what makes a cache hit when the model varies a field that does
// not change the answer.
func TestIgnoredArgumentsDoNotSplitTheCache(t *testing.T) {
	inner := &countingTool{
		result: "r",
		policy: types.CachePolicy{Enabled: true, TTL: time.Minute, IgnoreArgs: []string{"request_id"}},
	}
	cached, _ := New(inner, Config{Cache: newMemCache(), Scope: sessionScope()})

	cached.Execute(context.Background(), map[string]any{"q": "a", "request_id": "1"})
	cached.Execute(context.Background(), map[string]any{"q": "a", "request_id": "2"})
	if inner.count() != 1 {
		t.Errorf("tool ran %d times, want 1: request_id must not participate in the key", inner.count())
	}
}

// The forgettable one: a tool configured with root=/a must not serve results
// cached under root=/b.
func TestVaryOnContextSplitsTheCache(t *testing.T) {
	inner := &countingTool{
		result: "r",
		policy: types.CachePolicy{Enabled: true, TTL: time.Minute, VaryOnContext: []string{"root"}},
	}
	cached, _ := New(inner, Config{Cache: newMemCache(), Scope: sessionScope()})

	ctxA := types.WithToolContext(context.Background(), types.NewToolContext(map[string]any{"root": "/a"}))
	ctxB := types.WithToolContext(context.Background(), types.NewToolContext(map[string]any{"root": "/b"}))

	cached.Execute(ctxA, map[string]any{"q": "x"})
	cached.Execute(ctxB, map[string]any{"q": "x"})
	if inner.count() != 2 {
		t.Errorf("tool ran %d times, want 2: a context knob the result varies on must be in the key", inner.count())
	}

	cached.Execute(ctxA, map[string]any{"q": "x"})
	if inner.count() != 2 {
		t.Errorf("tool ran %d times, want still 2: returning to root=/a must hit", inner.count())
	}
}

func TestScopeSeparatesTenants(t *testing.T) {
	store := newMemCache()
	policy := types.CachePolicy{Enabled: true, TTL: time.Minute, Scope: types.CacheScopeUser}

	toolA := &countingTool{result: "a", policy: policy}
	toolB := &countingTool{result: "b", policy: policy}
	cachedA, _ := New(toolA, Config{Cache: store, Scope: map[types.CacheScope]string{types.CacheScopeUser: "alice"}})
	cachedB, _ := New(toolB, Config{Cache: store, Scope: map[types.CacheScope]string{types.CacheScopeUser: "bob"}})

	cachedA.Execute(context.Background(), map[string]any{"q": "x"})
	out, _ := cachedB.Execute(context.Background(), map[string]any{"q": "x"})

	if out != "b" {
		t.Errorf("bob got %q, want his own result: a user-scoped cache must never cross tenants", out)
	}
	if store.len() != 2 {
		t.Errorf("store holds %d entries, want 2 (one per user)", store.len())
	}
}

// Failing closed: a scoped policy with no scope key must refuse to build rather
// than quietly widening to a shared namespace.
func TestMissingScopeKeyIsAConstructionError(t *testing.T) {
	inner := &countingTool{policy: types.CachePolicy{Enabled: true, TTL: time.Minute, Scope: types.CacheScopeUser}}
	if _, err := New(inner, Config{Cache: newMemCache()}); err == nil {
		t.Error("a user-scoped policy with no user key must error, not fall back to a shared cache")
	}
}

func TestEnabledPolicyWithNoTTLIsRejected(t *testing.T) {
	inner := &countingTool{policy: types.CachePolicy{Enabled: true}}
	if _, err := New(inner, Config{Cache: newMemCache(), Scope: sessionScope()}); err == nil {
		t.Error("an unbounded tool cache must be rejected: it leaks memory and serves arbitrarily stale data")
	}
}

func TestDeclaredPolicyWithNoStoreIsAnError(t *testing.T) {
	inner := &countingTool{policy: types.CachePolicy{Enabled: true, TTL: time.Minute}}
	if _, err := New(inner, Config{Scope: sessionScope()}); err == nil {
		t.Error("declaring a cache policy with no backing store must error rather than silently not cache")
	}
}

// A transient failure cached for an hour turns one outage into an hour of one.
func TestErrorsAreNotCachedByDefault(t *testing.T) {
	inner := &countingTool{
		err:    errors.New("upstream down"),
		policy: types.CachePolicy{Enabled: true, TTL: time.Minute},
	}
	cached, _ := New(inner, Config{Cache: newMemCache(), Scope: sessionScope()})

	cached.Execute(context.Background(), map[string]any{"q": "x"})
	cached.Execute(context.Background(), map[string]any{"q": "x"})
	if inner.count() != 2 {
		t.Errorf("tool ran %d times, want 2: a failure must be retried, not served from cache", inner.count())
	}
}

func TestErrorsAreCachedWhenExplicitlyAllowed(t *testing.T) {
	inner := &countingTool{
		err:    errors.New("invalid argument"),
		policy: types.CachePolicy{Enabled: true, TTL: time.Minute, CacheErrors: true},
	}
	cached, _ := New(inner, Config{Cache: newMemCache(), Scope: sessionScope()})

	cached.Execute(context.Background(), map[string]any{"q": "x"})
	_, err := cached.Execute(context.Background(), map[string]any{"q": "x"})
	if inner.count() != 1 {
		t.Errorf("tool ran %d times, want 1 with CacheErrors set", inner.count())
	}
	if err == nil {
		t.Error("a cached failure must still be returned as a failure")
	}
}

func TestStaleEntryIsServedWhenRefreshFailsAndPolicyAllows(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }

	inner := &countingTool{
		result: "fresh",
		policy: types.CachePolicy{
			Enabled: true, TTL: time.Minute,
			ServeStaleOnError: true, MaxStale: time.Hour,
		},
	}
	cached, err := New(inner, Config{Cache: newMemCache(), Scope: sessionScope(), Now: clock})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := cached.Execute(context.Background(), map[string]any{"q": "x"}); err != nil {
		t.Fatal(err)
	}

	// Expire the entry, then break the tool.
	now = now.Add(2 * time.Minute)
	inner.err = errors.New("upstream down")
	inner.result = ""

	out, err := cached.Execute(context.Background(), map[string]any{"q": "x"})
	if err != nil {
		t.Fatalf("want the stale result rather than the error, got %v", err)
	}
	if out != "fresh" {
		t.Errorf("out = %q, want the stale cached value", out)
	}

	// Past MaxStale the answer is not degraded, it is wrong: the error wins.
	now = now.Add(2 * time.Hour)
	if _, err := cached.Execute(context.Background(), map[string]any{"q": "x"}); err == nil {
		t.Error("past MaxStale the error must propagate rather than serving an ancient result")
	}
}

func TestServeStaleWithoutMaxStaleIsRejected(t *testing.T) {
	inner := &countingTool{policy: types.CachePolicy{Enabled: true, TTL: time.Minute, ServeStaleOnError: true}}
	if _, err := New(inner, Config{Cache: newMemCache(), Scope: sessionScope()}); err == nil {
		t.Error("unbounded staleness must be rejected")
	}
}

// The agent fans tool calls out in parallel, so a cold cache would otherwise run
// the same expensive lookup several times at once.
func TestConcurrentIdenticalCallsCollapseToOneExecution(t *testing.T) {
	block := make(chan struct{})
	inner := &countingTool{
		result: "r",
		policy: types.CachePolicy{Enabled: true, TTL: time.Minute},
		block:  block,
	}
	cached, _ := New(inner, Config{Cache: newMemCache(), Scope: sessionScope()})

	const n = 8
	done := make(chan string, n)
	for i := 0; i < n; i++ {
		go func() {
			out, _ := cached.Execute(context.Background(), map[string]any{"q": "x"})
			done <- out
		}()
	}
	time.Sleep(20 * time.Millisecond) // let the goroutines reach the tool
	close(block)

	for i := 0; i < n; i++ {
		if out := <-done; out != "r" {
			t.Errorf("call %d returned %q, want r", i, out)
		}
	}
	if inner.count() != 1 {
		t.Errorf("tool ran %d times, want 1: identical concurrent calls must collapse", inner.count())
	}
}

func TestWrapAllLeavesUncachedToolsAlone(t *testing.T) {
	plain := &countingTool{result: "p"}
	src := types.NewToolRegistry(plain)

	out, err := WrapAll(src, Config{Cache: newMemCache(), Scope: sessionScope()})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := out.Get("search")
	if got != types.Tool(plain) {
		t.Error("a tool with no policy must pass through WrapAll unchanged")
	}
}
