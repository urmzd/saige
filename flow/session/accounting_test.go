package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/session"
)

func budgetSuite(t *testing.T, store session.BudgetStore) {
	t.Helper()
	ctx := context.Background()
	tenant := t.Name()
	if err := store.OpenBudget(ctx, tenant, "zero", session.Budget{}); !errors.Is(err, session.ErrBudget) {
		t.Fatalf("zero grant: %v", err)
	}
	limit := session.Budget{CostMicros: 100, Tokens: 100, Requests: 8}
	if err := store.OpenBudget(ctx, tenant, "root", limit); err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Go(func() {
			_, err := store.ReserveBudget(ctx, tenant, "root", fmt.Sprint(i), session.Budget{CostMicros: 10, Tokens: 1, Requests: 1})
			if err == nil {
				accepted.Add(1)
			} else if !errors.Is(err, session.ErrBudget) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if accepted.Load() != 8 {
		t.Fatalf("admitted %d, want exactly 8", accepted.Load())
	}
	a, err := store.BudgetAccount(ctx, tenant, "root")
	if err != nil {
		t.Fatal(err)
	}
	if a.Committed().Requests != 8 {
		t.Fatal(a)
	}
	for id, c := range a.Claims {
		if _, err := store.ReserveBudget(ctx, tenant, "root", id, c.Allocation); err != nil {
			t.Fatal("idempotent reserve", err)
		}
	}
	if err := store.CloseBudget(ctx, tenant, "root"); err != nil {
		t.Fatal(err)
	}
	a, _ = store.BudgetAccount(ctx, tenant, "root")
	if a.Ready() {
		t.Fatal("outstanding children marked settled")
	}
	for id := range a.Claims {
		actual := session.Budget{CostMicros: 3, Tokens: 1, Requests: 1}
		for range 2 {
			if err := store.SettleBudget(ctx, tenant, "root", id, actual); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.SettleBudget(ctx, tenant, "root", id, session.Budget{}); !errors.Is(err, session.ErrInvalid) {
			t.Fatal("receipt changed", err)
		}
	}
	a, _ = store.BudgetAccount(ctx, tenant, "root")
	if !a.Ready() || a.Spent().CostMicros != 24 || a.Spent().Requests != 8 {
		t.Fatal(a)
	}
	if _, err := store.ReserveBudget(ctx, tenant, "root", "after-close", session.Budget{Requests: 1}); !errors.Is(err, session.ErrBudget) {
		t.Fatal("closed reopened", err)
	}
	// CAS adapters persist this value, then reconstruct it after process restart.
	raw, _ := json.Marshal(a)
	var restored session.Account
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if !restored.Ready() || restored.Spent() != a.Spent() {
		t.Fatal("restart lost receipts")
	}
	if _, err := store.BudgetAccount(ctx, "another-tenant", "root"); !errors.Is(err, session.ErrNotFound) {
		t.Fatal("cross-tenant account", err)
	}
}
func TestAtomicBudgetReservationsAndImmutableReceipts(t *testing.T) {
	budgetSuite(t, session.NewMemory())
}

func usageSuite(t *testing.T, store session.Store) {
	t.Helper()
	ctx := context.Background()
	tenant := t.Name()
	u := store.(session.UsageStore)
	root, err := store.Open(ctx, tenant, session.Open{Parent: "console://tenant/root", Agent: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.Open(ctx, tenant, session.Open{Parent: root.ID, Agent: "child"})
	if err != nil {
		t.Fatal(err)
	}
	grandchild, err := store.Open(ctx, tenant, session.Open{Parent: child.ID, Agent: "grandchild"})
	if err != nil {
		t.Fatal(err)
	}
	own := event.UsageCall{AccountingID: "root/1", Requests: 1, PromptTokens: 2, CompletionTokens: 1, Cost: &event.Cost{Currency: "USD", Micros: 10}}
	if err := u.RecordUsage(ctx, tenant, root.ID, own); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Go(func() {
			c := own
			c.AccountingID = fmt.Sprintf("child/%d", i%8)
			target := child.ID
			if i%8 >= 4 {
				target = grandchild.ID
			}
			if err := u.RecordUsage(ctx, tenant, target, c); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	got, err := u.Usage(ctx, tenant, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Own.Requests != 1 || got.Children.Requests != 8 || got.Total.Requests != 9 || got.Total.Cost.Micros != 90 {
		t.Fatalf("root double counted: %+v", got)
	}
	got, err = u.Usage(ctx, tenant, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Own.Requests != 4 || got.Children.Requests != 4 || got.Total.Requests != 8 {
		t.Fatalf("child: %+v", got)
	}
	if _, err := u.Usage(ctx, "other", root.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatal("cross-tenant usage", err)
	}
}
func TestLateDescendantUsageRollsUpOnceDespiteConcurrentRedelivery(t *testing.T) {
	usageSuite(t, session.NewMemory())
}
