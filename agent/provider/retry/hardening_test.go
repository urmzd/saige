package retry

import (
	"context"
	"errors"
	"math/rand/v2"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

func TestDelay(t *testing.T) {
	base := Config{MaxAttempts: 5, BaseDelay: 100 * time.Millisecond, MaxDelay: time.Second, Multiplier: 2, MaxRetryAfter: 5 * time.Second}
	plain := errors.New("x")
	after := func(d time.Duration) error {
		return &types.ProviderError{Kind: types.ErrorKindRateLimit, RetryAfter: d}
	}
	for _, tc := range []struct {
		name    string
		jitter  bool
		attempt int
		err     error
		u       float64
		want    time.Duration
	}{
		{"exponential without jitter", false, 2, plain, 0.5, 400 * time.Millisecond},
		{"capped at max delay", false, 10, plain, 0.5, time.Second},
		{"full jitter scales the backoff", true, 2, plain, 0.25, 100 * time.Millisecond},
		{"jitter can reach zero", true, 0, plain, 0, 0},
		{"retry after is a floor", true, 0, after(2 * time.Second), 0.1, 2 * time.Second},
		{"retry after exceeds max delay", false, 0, after(3 * time.Second), 0, 3 * time.Second},
		{"retry after is clamped", false, 0, after(time.Minute), 0, 5 * time.Second},
		{"shorter retry after keeps backoff", false, 2, after(time.Millisecond), 0, 400 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			cfg.DisableJitter = !tc.jitter
			if got := must.Get(New(&plainProvider{}, cfg)).delay(tc.attempt, tc.err, tc.u); got != tc.want {
				t.Fatalf("delay = %v, want %v", got, tc.want)
			}
			if got := cfg.Delay(tc.attempt, tc.err, tc.u); got != tc.want {
				t.Fatalf("Config.Delay = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestJitterSamplesSpread(t *testing.T) {
	p := must.Get(New(&plainProvider{}, Config{BaseDelay: 100 * time.Millisecond, MaxDelay: time.Second}))
	limit := 200 * time.Millisecond
	seen := map[time.Duration]bool{}
	for range 100 {
		d := p.delay(1, errors.New("x"), randFloat())
		if d < 0 || d > limit {
			t.Fatalf("delay %v outside [0, %v]", d, limit)
		}
		seen[d] = true
	}
	if len(seen) < 2 {
		t.Fatal("jittered delays are all equal")
	}
}

func TestBackoffHonorsRetryAfter(t *testing.T) {
	var calls atomic.Int32
	inner := &countingProvider{calls: &calls, failUntil: 1, response: "ok",
		err: &types.ProviderError{Kind: types.ErrorKindRateLimit, RetryAfter: 80 * time.Millisecond, Err: errors.New("429")}}
	p := must.Get(New(inner, Config{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}))
	start := time.Now()
	ch, err := p.Stream(context.Background(), types.Request{})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
		t.Fatalf("waited %v, want at least the 80ms Retry-After", elapsed)
	}
}

type plainProvider struct{ calls atomic.Int32 }

func (p *plainProvider) Stream(_ context.Context, _ types.Request) (<-chan types.Delta, error) {
	p.calls.Add(1)
	ch := make(chan types.Delta)
	close(ch)
	return ch, nil
}

func TestSchemaIsNeverDropped(t *testing.T) {
	inner := &plainProvider{}
	p := must.Get(New(inner, DefaultConfig()))
	_, err := p.Stream(context.Background(), types.Request{Schema: &types.ParameterSchema{Type: "object"}})
	if !errors.Is(err, types.ErrInvalidModelConfig) || types.IsTransient(err) || inner.calls.Load() != 0 {
		t.Fatalf("err = %v calls = %d, want a local rejection", err, inner.calls.Load())
	}
	if _, err := p.Stream(context.Background(), types.Request{}); err != nil || inner.calls.Load() != 1 {
		t.Fatalf("a nil schema must pass through: err = %v", err)
	}
}

// endlessProvider sends n deltas on an unbuffered channel, ignoring ctx, and
// closes finished when it returns. lead is sent first.
type endlessProvider struct {
	lead     []types.Delta
	n        int
	finished chan struct{}
}

func (p *endlessProvider) Stream(_ context.Context, _ types.Request) (<-chan types.Delta, error) {
	ch := make(chan types.Delta)
	go func() {
		defer close(p.finished)
		defer close(ch)
		for _, d := range p.lead {
			ch <- d
		}
		for range p.n {
			ch <- types.PartDelta{Index: 0, Text: "x"}
		}
	}()
	return ch, nil
}

func waitFinished(t *testing.T, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("producer goroutine leaked: still blocked on send")
	}
}

func TestConsumerCancelDoesNotLeak(t *testing.T) {
	before := runtime.NumGoroutine()
	inner := &endlessProvider{lead: []types.Delta{types.PartStart{Index: 0, Kind: types.KindText}}, n: 1000, finished: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := must.Get(New(inner, DefaultConfig())).Stream(ctx, types.Request{})
	if err != nil {
		t.Fatal(err)
	}
	<-ch
	cancel() // stop reading without draining
	waitFinished(t, inner.finished)
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		t.Fatalf("goroutines = %d, want <= %d", n, before)
	}
}

// chattyFailure fails once with an ErrorDelta, then keeps sending, then
// succeeds on the next call.
type chattyFailure struct {
	calls    atomic.Int32
	finished chan struct{}
}

func (p *chattyFailure) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	m, tl := req.Messages, req.Tools
	if p.calls.Add(1) == 1 {
		return (&endlessProvider{
			lead:     []types.Delta{types.ErrorDelta{Error: transientErr()}},
			n:        100,
			finished: p.finished,
		}).Stream(ctx, types.Request{Messages: m, Tools: tl})
	}
	return (&mockProvider{response: "ok"}).Stream(ctx, types.Request{Messages: m, Tools: tl})
}

func TestRetriedAttemptIsDrained(t *testing.T) {
	inner := &chattyFailure{finished: make(chan struct{})}
	ch, err := must.Get(New(inner, Config{MaxAttempts: 2, BaseDelay: time.Millisecond})).Stream(context.Background(), types.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if text, _, firstErr := collect(ch); text != "ok" || firstErr != nil {
		t.Fatalf("text = %q err = %v", text, firstErr)
	}
	waitFinished(t, inner.finished)
}

func randFloat() float64 { return rand.Float64() }
