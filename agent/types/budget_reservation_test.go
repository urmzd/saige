package types

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestBudgetAtomicAdmission(t *testing.T) {
	b := NewBudget(BudgetPolicy{Limit: USD(1), MaxRequests: 4, MaxTokens: 400, PerCallTokens: 100, PerCallCost: USD(.25)})
	p := Pricing{Free: true}
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := b.Reserve(NewID(), p)
			if err == nil {
				admitted.Add(1)
			} else if !errors.Is(err, ErrBudgetBusy) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if admitted.Load() != 4 || b.Remaining() != 0 {
		t.Fatalf("admitted=%d remaining=%v", admitted.Load(), b.Remaining())
	}
}
func TestBudgetSettlementIsOnceAndUnknownIsCharged(t *testing.T) {
	b := NewBudget(BudgetPolicy{Limit: USD(1), MaxTokens: 1000, PerCallCost: USD(.25), PerCallTokens: 100})
	p := Pricing{InputPerMTok: 1}
	if _, err := b.Reserve("a", p); err != nil {
		t.Fatal(err)
	}
	if err := b.Settle("a", "model", p, TokenUsage{}, true); err != nil {
		t.Fatal(err)
	}
	if err := b.Settle("a", "model", p, TokenUsage{}, true); err != nil {
		t.Fatal(err)
	}
	if b.Spent() != USD(.25) || b.Usage().Total() != 100 || b.Usage().Requests != 1 || b.Uncertain() != 1 {
		t.Fatalf("cost=%v usage=%+v", b.Spent(), b.Usage())
	}
	if _, err := b.Reserve("b", p); err != nil {
		t.Fatal(err)
	}
	if err := b.Settle("b", "model", p, TokenUsage{}, false); err != nil {
		t.Fatal(err)
	} // response cache
	if b.Remaining() != USD(.75) {
		t.Fatal(b.Remaining())
	}
}
func TestCumulativeUsageDoesNotDoubleCount(t *testing.T) {
	u := UsageDelta{}
	for _, d := range []UsageDelta{{PromptTokens: 100, CompletionTokens: 2, Cumulative: true}, {PromptTokens: 100, CompletionTokens: 12, CachedPromptTokens: 30, Cumulative: true}, {PromptTokens: 100, CompletionTokens: 12, CachedPromptTokens: 30, Cumulative: true}} {
		u = u.Merge(d)
	}
	if u.PromptTokens != 100 || u.CompletionTokens != 12 || u.CachedPromptTokens != 30 || u.TotalTokens != 112 {
		t.Fatalf("%+v", u)
	}
}
