package sink_test

import (
	"context"
	"errors"
	"testing"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connectors/internal/sink"
	"github.com/urmzd/saige/flow/deliverystate"
	"github.com/urmzd/saige/flow/event"
)

// memoSurface fails its first create after leaving a memo, and records the
// memo each create is given.
type memoSurface struct {
	given []string
}

func (m *memoSurface) Create(context.Context, address.Address, string) (string, error) {
	return "", errors.New("Create called instead of CreateMemo")
}

func (m *memoSurface) CreateMemo(_ context.Context, _ address.Address, _ string, memo string) (string, string, error) {
	m.given = append(m.given, memo)
	if len(m.given) == 1 {
		return "", "decided", errors.New("response lost")
	}
	return "m1", memo + "+posted", nil
}

func (m *memoSurface) Update(context.Context, address.Address, string, string) error { return nil }

// A memo a failed create leaves is kept with the delivery's state and given
// to the retried create, also on another replica.
func TestMemoSurvivesAFailedCreate(t *testing.T) {
	ctx := context.Background()
	store := deliverystate.NewMemory()
	surface := &memoSurface{}
	replica := func() *sink.Engine {
		return sink.New(surface, layout, sink.WithState(store, "github:client1"), sink.WithInterval(0))
	}
	if err := replica().Deliver(ctx, target, text(1, "hello")); err == nil {
		t.Fatal("a failed create was delivered")
	}
	if err := replica().Deliver(ctx, target, ev(2, event.TypeCompleted, "")); err != nil {
		t.Fatal(err)
	}
	if len(surface.given) != 2 || surface.given[0] != "" || surface.given[1] != "decided" {
		t.Fatalf("creates were given %q, want \"\" then the kept memo", surface.given)
	}
}
