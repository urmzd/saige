//go:build stress

package stress

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/provider/anthropic"
	"github.com/urmzd/saige/agent/provider/openai"
	"github.com/urmzd/saige/agent/provider/retry"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

// burstStats aggregates what one model's burst saw at the adapter.
type burstStats struct {
	mu                sync.Mutex
	attempts          int
	rateLimited       int // 429 and quota errors
	otherErrors       int
	retryAfterSeen    int // errors that carried Retry-After
	retryAfterWaited  int // attempts that followed such an error
	retryAfterHonored int // of those, attempts that waited long enough
	successes         int
	failures          int
	promptTokens      int
	completionTokens  int
	latencies         []time.Duration
}

// countingProvider sits between the retry wrapper and the adapter. It counts
// every attempt and its error, and checks that an attempt after an error
// carrying Retry-After starts no sooner than that delay. Each logical call
// gets its own instance, so attempts arrive one at a time.
type countingProvider struct {
	inner      types.Provider
	stats      *burstStats
	lastErrAt  time.Time
	retryAfter time.Duration
}

func (p *countingProvider) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	msgs, tools := req.Messages, req.Tools
	now := time.Now()
	p.stats.mu.Lock()
	p.stats.attempts++
	if p.retryAfter > 0 {
		p.stats.retryAfterWaited++
		if now.Sub(p.lastErrAt) >= p.retryAfter {
			p.stats.retryAfterHonored++
		}
	}
	p.stats.mu.Unlock()
	p.retryAfter = 0

	ch, err := p.inner.Stream(ctx, types.Request{Messages: msgs, Tools: tools})
	if err != nil {
		p.record(err)
		return nil, err
	}
	out := make(chan types.Delta)
	go func() {
		defer close(out)
		for d := range ch {
			if ed, ok := d.(types.ErrorDelta); ok {
				p.record(ed.Error)
			}
			if u, ok := d.(types.UsageDelta); ok {
				p.stats.mu.Lock()
				p.stats.promptTokens += u.PromptTokens
				p.stats.completionTokens += u.CompletionTokens
				p.stats.mu.Unlock()
			}
			out <- d
		}
	}()
	return out, nil
}

func (p *countingProvider) record(err error) {
	p.lastErrAt = time.Now()
	p.retryAfter = types.RetryAfter(err)
	p.stats.mu.Lock()
	defer p.stats.mu.Unlock()
	if types.IsRateLimit(err) {
		p.stats.rateLimited++
	} else {
		p.stats.otherErrors++
	}
	if p.retryAfter > 0 {
		p.stats.retryAfterSeen++
	}
}

// TestLiveBurst sends a burst of parallel one-word requests to each small
// model through a retry-wrapped adapter and reports rate limits, retries and
// Retry-After handling. It needs SAIGE_LIVE=1 and the provider keys; each
// request is a few dozen tokens, so a run costs well under a cent.
func TestLiveBurst(t *testing.T) {
	if os.Getenv("SAIGE_LIVE") != "1" {
		t.Skip("SAIGE_LIVE=1 not set; skipping live burst")
	}
	burst := envInt(t, "SAIGE_STRESS_BURST", 20)
	models := []struct {
		name, key string
		build     func(key string) types.Provider
	}{
		{"gpt-6-luna", "OPENAI_API_KEY", func(key string) types.Provider {
			return must.Get(openai.New(openai.Config{APIKey: key, Model: "gpt-6-luna"}, openai.WithMaxTokens(32), openai.WithReasoningEffort("none")))
		}},
		{"claude-haiku-5-5", "ANTHROPIC_API_KEY", func(key string) types.Provider {
			return must.Get(anthropic.New(anthropic.Config{APIKey: key, Model: "claude-haiku-5-5"}, anthropic.WithMaxTokens(32)))
		}},
	}
	cfg := retry.Config{MaxAttempts: 4, BaseDelay: 500 * time.Millisecond, MaxDelay: 8 * time.Second, MaxRetryAfter: 30 * time.Second}
	const prompt = "Reply with the single word: ok"

	for _, m := range models {
		t.Run(m.name, func(t *testing.T) {
			key := os.Getenv(m.key)
			if key == "" {
				t.Skipf("%s not set", m.key)
			}
			adapter := m.build(key)
			stats := &burstStats{}
			ctx := testContext(t, 2*time.Minute)
			var wg sync.WaitGroup
			start := time.Now()
			for range burst {
				wg.Add(1)
				go func() {
					defer wg.Done()
					p := must.Get(retry.New(&countingProvider{inner: adapter, stats: stats}, cfg))
					began := time.Now()
					text, err := types.GenerateText(ctx, p, prompt)
					stats.mu.Lock()
					defer stats.mu.Unlock()
					if err != nil || text == "" {
						stats.failures++
						t.Logf("call failed: text=%q err=%v", text, err)
						return
					}
					stats.successes++
					stats.latencies = append(stats.latencies, time.Since(began))
				}()
			}
			wg.Wait()
			wall := time.Since(start)

			if len(stats.latencies) > 0 {
				logLatency(t, m.name+" calls", stats.latencies, wall)
			}
			retries := stats.attempts - burst
			t.Logf("%s: calls=%d successes=%d failures=%d attempts=%d retries=%d 429s=%d other errors=%d retry-after seen=%d honored=%d/%d tokens in=%d out=%d",
				m.name, burst, stats.successes, stats.failures, stats.attempts, retries, stats.rateLimited,
				stats.otherErrors, stats.retryAfterSeen, stats.retryAfterHonored, stats.retryAfterWaited, stats.promptTokens, stats.completionTokens)
			if stats.retryAfterHonored != stats.retryAfterWaited {
				t.Errorf("Retry-After honored on %d of %d retried attempts", stats.retryAfterHonored, stats.retryAfterWaited)
			}
			if stats.successes == 0 {
				t.Fatal("no call succeeded")
			}
		})
	}
}
