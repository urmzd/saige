package preset

import (
	"context"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/types"
)

type hanging struct{}

func (hanging) Stream(ctx context.Context, _ types.Request) (<-chan types.Delta, error) {
	ch := make(chan types.Delta)
	go func() {
		<-ctx.Done()
		ch <- types.ErrorDelta{Error: ctx.Err()}
		close(ch)
	}()
	return ch, nil
}

func TestAttemptTimeoutIsTransient(t *testing.T) {
	p := withAttemptTimeout(hanging{}, 10*time.Millisecond)
	ch, err := p.Stream(context.Background(), types.Request{})
	if err != nil {
		t.Fatal(err)
	}
	var got error
	for d := range ch {
		if e, ok := d.(types.ErrorDelta); ok {
			got = e.Error
		}
	}
	if !types.IsTransient(got) {
		t.Fatalf("got %v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch, _ = p.Stream(ctx, types.Request{})
	cancel()
	for d := range ch {
		if e, ok := d.(types.ErrorDelta); ok && types.IsTransient(e.Error) {
			t.Fatal("a caller cancellation must not read as an attempt timeout")
		}
	}
}
