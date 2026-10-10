package router

import (
	"context"
	"errors"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

// BenchmarkRouterOverhead measures routing one request through a router
// session, on the primary profile and when the primary fails over to the
// secondary.
func BenchmarkRouterOverhead(b *testing.B) {
	ok := func() (<-chan types.Delta, error) {
		return deltas(types.TextStartDelta{}, types.TextContentDelta{Content: "ok"}, types.TextEndDelta{}), nil
	}
	busy := func() (<-chan types.Delta, error) {
		return nil, &types.ProviderError{Kind: types.ErrorKindTransient, Err: errors.New("busy")}
	}
	for _, c := range []struct {
		name    string
		primary func() (<-chan types.Delta, error)
	}{{"primary", ok}, {"failover", busy}} {
		b.Run(c.name, func(b *testing.B) {
			r, err := New(Config{Profiles: []Profile{
				{ID: "a", Provider: provider{model: "a", call: c.primary}},
				{ID: "b", Provider: provider{model: "b", call: ok}},
			}})
			if err != nil {
				b.Fatal(err)
			}
			msgs := []types.Message{types.NewUserMessage("hi")}
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					ch, err := r.Session().ChatStream(context.Background(), msgs, nil)
					if err != nil {
						b.Fatal(err)
					}
					for range ch {
					}
				}
			})
		})
	}
}
