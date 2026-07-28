package types

import (
	"errors"
	"testing"
)

func gpt4oPricing() Pricing {
	return Pricing{Currency: "USD", InputPerMTok: 2.50, OutputPerMTok: 10, AsOf: "2026-07-24"}
}

func TestCostArithmeticIsExactAtScale(t *testing.T) {
	p := gpt4oPricing()
	// One million input tokens at 2.50/Mtok is exactly 2.50.
	if got := p.Cost(TokenUsage{InputTokens: 1_000_000}); got != USD(2.50) {
		t.Errorf("cost = %s, want 2.50", got)
	}

	// Ten thousand small calls must sum exactly, which is the whole reason Cost
	// is an integer: float accumulation drifts and a drifting budget does not
	// stop where it was told to.
	var total Cost
	for i := 0; i < 10_000; i++ {
		total += p.Cost(TokenUsage{InputTokens: 100, OutputTokens: 100})
	}
	want := p.Cost(TokenUsage{InputTokens: 1_000_000, OutputTokens: 1_000_000})
	if total != want {
		t.Errorf("summed cost = %s, want %s", total, want)
	}
}

func TestUnpricedIsNotFree(t *testing.T) {
	var unpriced Pricing
	if !unpriced.IsZero() {
		t.Error("an all-zero rate card must report as unpriced")
	}

	free := Pricing{Free: true}
	if free.IsZero() {
		t.Error("an explicitly free model must not report as unpriced: a budget treats the two differently")
	}
	if got := free.Cost(TokenUsage{InputTokens: 1_000_000}); got != 0 {
		t.Errorf("free cost = %s, want 0", got)
	}
}

func TestBudgetStopsAtTheLimit(t *testing.T) {
	b := NewBudget(BudgetPolicy{Limit: USD(1.00), OnExceed: BudgetStop})
	p := gpt4oPricing()

	// 0.25 per call at these rates.
	for i := 0; i < 3; i++ {
		status, err := b.Record("gpt-4o", p, TokenUsage{InputTokens: 100_000, Requests: 1})
		if err != nil {
			t.Fatalf("Record: %v", err)
		}
		if status == BudgetStatusExceeded {
			t.Fatalf("exceeded after %d calls, too early", i+1)
		}
	}
	if err := b.Err(); err != nil {
		t.Fatalf("under the limit the budget must not error: %v", err)
	}

	status, _ := b.Record("gpt-4o", p, TokenUsage{InputTokens: 100_000, Requests: 1})
	if status != BudgetStatusExceeded {
		t.Fatalf("status = %v after spending the full limit, want exceeded", status)
	}
	if err := b.Err(); !errors.Is(err, ErrBudgetExceeded) {
		t.Errorf("Err = %v, want ErrBudgetExceeded", err)
	}
	if got := b.Remaining(); got != 0 {
		t.Errorf("Remaining = %s, want 0", got)
	}
}

func TestBudgetWarnsBeforeStopping(t *testing.T) {
	b := NewBudget(BudgetPolicy{Limit: USD(1.00), WarnAt: 0.5, OnExceed: BudgetStop})
	p := gpt4oPricing()

	status, _ := b.Record("gpt-4o", p, TokenUsage{InputTokens: 100_000, Requests: 1}) // 0.25
	if status != BudgetStatusOK {
		t.Errorf("status at 25%% = %v, want ok", status)
	}
	status, _ = b.Record("gpt-4o", p, TokenUsage{InputTokens: 120_000, Requests: 1}) // 0.55 total
	if status != BudgetStatusWarn {
		t.Errorf("status at 55%% = %v, want warn", status)
	}
	if err := b.Err(); err != nil {
		t.Error("a warning must not stop the run")
	}
}

// Approving a spend increase must buy a bounded grant, not remove the ceiling:
// one "yes" turning into an unbounded run is exactly what the approver is
// trying to avoid.
func TestApprovalGrantsMoreAllowanceRatherThanRemovingTheLimit(t *testing.T) {
	b := NewBudget(BudgetPolicy{
		Limit: USD(1.00), OnExceed: BudgetRequireApproval, ApprovalGrant: USD(0.50),
	})
	p := gpt4oPricing()

	_, _ = b.Record("gpt-4o", p, TokenUsage{InputTokens: 400_000, Requests: 1}) // 1.00
	if b.Status() != BudgetStatusExceeded {
		t.Fatal("want exceeded at the limit")
	}
	// Under RequireApproval the budget does not stop on its own.
	if err := b.Err(); err != nil {
		t.Errorf("Err = %v, want nil under BudgetRequireApproval: the caller escalates instead", err)
	}

	b.Grant(0) // apply the policy's grant
	if b.Status() != BudgetStatusOK {
		t.Error("after a grant the run must be able to continue")
	}
	if got := b.Remaining(); got != USD(0.50) {
		t.Errorf("Remaining = %s, want the 0.50 grant", got)
	}

	_, _ = b.Record("gpt-4o", p, TokenUsage{InputTokens: 200_000, Requests: 1}) // 0.50 more
	if b.Status() != BudgetStatusExceeded {
		t.Error("the grant must itself be bounded: spending it must exceed again")
	}
}

func TestUnpricedModelIsRejectedUnderAnEnforcingPolicy(t *testing.T) {
	b := NewBudget(BudgetPolicy{Limit: USD(1.00)})
	_, err := b.Record("mystery-1", Pricing{}, TokenUsage{InputTokens: 1000, Requests: 1})
	if !errors.Is(err, ErrUnpriced) {
		t.Errorf("err = %v, want ErrUnpriced: an uncostable model cannot be held to a limit", err)
	}

	// Usage is still recorded so a report shows what was spent.
	if b.Usage().InputTokens != 1000 {
		t.Error("usage must be recorded even when it cannot be costed")
	}

	allowed := NewBudget(BudgetPolicy{Limit: USD(1.00), AllowUnpriced: true})
	if _, err := allowed.Record("mystery-1", Pricing{}, TokenUsage{InputTokens: 1000, Requests: 1}); err != nil {
		t.Errorf("AllowUnpriced must permit the call: %v", err)
	}
}

func TestRequestAndTokenCeilingsBackstopMispricing(t *testing.T) {
	b := NewBudget(BudgetPolicy{MaxRequests: 2})
	free := Pricing{Free: true}

	_, _ = b.Record("local", free, TokenUsage{Requests: 1})
	if b.Exceeded() {
		t.Fatal("one request must not trip a two-request cap")
	}
	_, _ = b.Record("local", free, TokenUsage{Requests: 1})
	if !b.Exceeded() {
		t.Error("a request ceiling must apply even when every call is free")
	}

	tok := NewBudget(BudgetPolicy{MaxTokens: 1000})
	_, _ = tok.Record("local", free, TokenUsage{InputTokens: 1000, Requests: 1})
	if !tok.Exceeded() {
		t.Error("a token ceiling must apply independently of cost")
	}
}

func TestZeroPolicyTracksWithoutLimiting(t *testing.T) {
	b := NewBudget(BudgetPolicy{})
	p := gpt4oPricing()
	for i := 0; i < 100; i++ {
		if _, err := b.Record("gpt-4o", p, TokenUsage{InputTokens: 1_000_000, Requests: 1}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if b.Exceeded() {
		t.Error("a zero policy must not limit anything")
	}
	if b.Remaining() != -1 {
		t.Errorf("Remaining = %s, want -1 for unlimited", b.Remaining())
	}
	if b.Spent() != USD(250) {
		t.Errorf("Spent = %s, want 250.00 tracked even without a limit", b.Spent())
	}
}

func TestBreakdownAttributesSpendPerModel(t *testing.T) {
	b := NewBudget(BudgetPolicy{})
	_, _ = b.Record("gpt-4o", gpt4oPricing(), TokenUsage{InputTokens: 1_000_000, Requests: 1})
	_, _ = b.Record("local", Pricing{Free: true}, TokenUsage{InputTokens: 5_000_000, Requests: 4})

	byModel := map[string]Report{}
	for _, r := range b.Breakdown() {
		byModel[r.Model] = r
	}
	if got := byModel["gpt-4o"].Cost; got != USD(2.50) {
		t.Errorf("gpt-4o cost = %s, want 2.50", got)
	}
	if got := byModel["local"].Cost; got != 0 {
		t.Errorf("local cost = %s, want 0", got)
	}
	if got := byModel["local"].Usage.Requests; got != 4 {
		t.Errorf("local requests = %d, want 4", got)
	}
}

func TestConcurrentRecordsAllCount(t *testing.T) {
	b := NewBudget(BudgetPolicy{})
	p := gpt4oPricing()
	const n = 100
	done := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		go func() {
			_, _ = b.Record("gpt-4o", p, TokenUsage{InputTokens: 10_000, Requests: 1})
			done <- struct{}{}
		}()
	}
	for i := 0; i < n; i++ {
		<-done
	}
	if got := b.Usage().Requests; got != n {
		t.Errorf("requests = %d, want %d: parallel tool and sub-agent calls must all count", got, n)
	}
}

func TestBreakdownNamesTheCurrencyItCounted(t *testing.T) {
	b := NewBudget(BudgetPolicy{})
	_, _ = b.Record("gpt-4o", gpt4oPricing(), TokenUsage{InputTokens: 10_000, Requests: 1})
	_, _ = b.Record("local", Pricing{Free: true}, TokenUsage{InputTokens: 10_000, Requests: 1})
	_, _ = b.Record("mystery", Pricing{}, TokenUsage{InputTokens: 10_000, Requests: 1})

	got := map[string]string{}
	for _, r := range b.Breakdown() {
		got[r.Model] = r.Currency
	}

	// A priced model names its unit: a bare number is not a spend report.
	if got["gpt-4o"] != DefaultCurrency {
		t.Errorf("gpt-4o currency = %q, want USD", got["gpt-4o"])
	}
	if got["local"] != DefaultCurrency {
		t.Errorf("free model currency = %q, want USD: free is priced at zero, not unpriced", got["local"])
	}
	// An unpriced model has no unit, and an empty string is how a report says
	// "this zero means unknown" rather than "this zero means nothing was spent".
	if got["mystery"] != "" {
		t.Errorf("unpriced model currency = %q, want empty", got["mystery"])
	}
}
