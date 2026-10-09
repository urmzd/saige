package pgstore

import (
	"context"
	"errors"
	"testing"

	"github.com/urmzd/saige/agent/store/storetest"
	"github.com/urmzd/saige/agent/types"
)

// TestConformance runs the shared Store suite against PostgreSQL. Stores for
// different conversations share one pool, so scoping is exercised for real.
func TestConformance(t *testing.T) {
	pool := testPool(t)
	storetest.RunConformance(t, func(t *testing.T, conversationID string) types.Store {
		return NewStore(pool, conversationID, nil)
	})
}

// TestSaveNodeRejectsOtherConversationsNode covers a write, not just a read,
// across the conversation boundary: B must not overwrite A's node even with a
// higher version.
func TestSaveNodeRejectsOtherConversationsNode(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	a := NewStore(pool, "conv-a", nil)
	b := NewStore(pool, "conv-b", nil)
	if err := a.SaveNode(ctx, testNode("shared", "", "main", 0)); err != nil {
		t.Fatal(err)
	}
	newer := testNode("shared", "", "main", 0)
	newer.Version = 5
	if err := b.SaveNode(ctx, newer); !errors.Is(err, ErrConversationMismatch) {
		t.Fatalf("cross-conversation SaveNode error = %v, want ErrConversationMismatch", err)
	}
	got, err := a.LoadNode(ctx, "shared")
	if err != nil || got.Version != 1 {
		t.Fatalf("A's node after B's write = %+v, %v", got, err)
	}
}
