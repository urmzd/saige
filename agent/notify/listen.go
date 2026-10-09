package notify

import (
	"context"
	"sync"

	"github.com/urmzd/saige/agent/types"
)

// Listen subscribes to channel and calls fn for each notification in a
// goroutine until stop is called, ctx ends, or the notifier closes the
// subscription. Calls to fn are sequential. stop waits for a running fn to
// return.
//
// It turns a broadcast into a trigger. To reload the model catalog on every
// process when one process changes its source:
//
//	stop, err := notify.Listen(ctx, n, "saige.catalog", func(ctx context.Context, _ types.Notification) {
//		if _, _, err := catalog.Refresh(ctx, src); err != nil {
//			slog.Warn("catalog refresh", "err", err)
//		}
//	})
//	// After updating the source: n.Publish(ctx, "saige.catalog", nil)
func Listen(ctx context.Context, n types.Notifier, channel string, fn func(context.Context, types.Notification)) (stop func(), err error) {
	ctx, cancelCtx := context.WithCancel(ctx)
	ch, cancel, err := n.Subscribe(ctx, channel)
	if err != nil {
		cancelCtx()
		return nil, err
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for msg := range ch {
			if ctx.Err() != nil {
				continue // drain until the subscription closes
			}
			fn(ctx, msg)
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancelCtx()
			cancel()
			wg.Wait()
		})
	}, nil
}
