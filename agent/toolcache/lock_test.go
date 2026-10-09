package toolcache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// slowCache delays every Get, standing in for a remote cache, and can fail
// reads on demand.
type slowCache struct {
	*memCache
	delay  time.Duration
	getErr error
}

func (s *slowCache) Get(ctx context.Context, key string) (Entry, bool, error) {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return Entry{}, false, ctx.Err()
	}
	if s.getErr != nil {
		return Entry{}, false, s.getErr
	}
	return s.memCache.Get(ctx, key)
}

// Misses on different keys must not queue behind each other's cache reads.
func TestCacheReadsDoNotSerializeAcrossKeys(t *testing.T) {
	const calls = 10
	const delay = 60 * time.Millisecond
	inner := &countingTool{result: "r", policy: types.CachePolicy{Enabled: true, TTL: time.Minute}}
	cached, err := New(inner, Config{Cache: &slowCache{memCache: newMemCache(), delay: delay}, Scope: sessionScope()})
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := cached.Execute(context.Background(), map[string]any{"q": fmt.Sprint(i)}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()

	// Each call does two reads (the first lookup and the leader's re-check).
	// Serialized re-checks would take calls*delay on top of that.
	if elapsed := time.Since(start); elapsed > time.Duration(calls/2)*delay {
		t.Errorf("10 concurrent misses took %v; cache reads are serialized", elapsed)
	}
	if got := inner.count(); got != calls {
		t.Errorf("tool ran %d times, want %d", got, calls)
	}
}

func TestCacheReadFailuresAreLogged(t *testing.T) {
	tests := []struct {
		name    string
		getErr  error
		wantLog bool
	}{
		{"backend error", errors.New("connection refused"), true},
		{"caller cancelled", context.Canceled, false},
		{"healthy cache", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, nil))
			inner := &countingTool{result: "r", policy: types.CachePolicy{Enabled: true, TTL: time.Minute}}
			cached, err := New(inner, Config{
				Cache:  &slowCache{memCache: newMemCache(), getErr: tt.getErr},
				Scope:  sessionScope(),
				Logger: logger,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := cached.Execute(context.Background(), map[string]any{"q": "x"}); err != nil {
				t.Fatal(err)
			}
			got := strings.Contains(buf.String(), "cache read failed")
			if got != tt.wantLog {
				t.Errorf("logged=%v, want %v; log:\n%s", got, tt.wantLog, buf.String())
			}
		})
	}
}
