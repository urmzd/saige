package retry

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// textProvider streams a short text answer. When failEvery is set, every
// failEvery-th call fails with a transient error first.
type textProvider struct {
	failEvery int64
	calls     atomic.Int64
}

func (p *textProvider) Stream(_ context.Context, _ types.Request) (<-chan types.Delta, error) {
	if n := p.calls.Add(1); p.failEvery > 0 && n%p.failEvery == 0 {
		return nil, &types.ProviderError{Kind: types.ErrorKindTransient, Err: errors.New("busy")}
	}
	ch := make(chan types.Delta, 3)
	ch <- types.PartStart{Index: 0, Kind: types.KindText}
	ch <- types.PartDelta{Index: 0, Text: "ok"}
	ch <- types.PartEnd{Index: 0}
	close(ch)
	return ch, nil
}

// benchStream streams through a provider from newProvider on every P. Each
// goroutine builds its own, so a failing call is always followed by a
// succeeding one on the same goroutine.
func benchStream(b *testing.B, newProvider func() types.Provider) {
	b.Helper()
	msgs := []types.Message{types.UserMsg(types.Text("hi"))}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		p := newProvider()
		for pb.Next() {
			ch, err := p.Stream(context.Background(), types.Request{Messages: msgs})
			if err != nil {
				b.Fatal(err)
			}
			for range ch {
			}
		}
	})
}

// BenchmarkRetryOverhead compares a bare provider with the same provider
// behind the retry wrapper, on the success path and with one call in two
// failing once. Backoff is a nanosecond, so the numbers show the wrapper's
// own cost rather than its sleeps.
func BenchmarkRetryOverhead(b *testing.B) {
	cfg := Config{MaxAttempts: 3, BaseDelay: time.Nanosecond, MaxDelay: time.Nanosecond, DisableJitter: true}
	b.Run("direct", func(b *testing.B) {
		benchStream(b, func() types.Provider { return &textProvider{} })
	})
	b.Run("retry", func(b *testing.B) {
		benchStream(b, func() types.Provider { return New(&textProvider{}, cfg) })
	})
	b.Run("retry-transient", func(b *testing.B) {
		benchStream(b, func() types.Provider { return New(&textProvider{failEvery: 2}, cfg) })
	})
}
