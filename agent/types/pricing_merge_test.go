package types

import "testing"

// A fallback chain reports the worse of its members' rate cards. "Worse" has to
// mean the one that cannot under-count, and the two states that are easy to get
// backwards are unpriced (unknown) and free (known to be zero).

func TestUnpricedMemberMakesTheChainUnpriced(t *testing.T) {
	paid := Pricing{InputPerMTok: 3, OutputPerMTok: 15, AsOf: "2026-07-01", Source: "vendor list price"}

	for _, tc := range []struct {
		name string
		a, b Pricing
	}{
		{"unpriced primary", Pricing{}, paid},
		{"unpriced secondary", paid, Pricing{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := worsePricing(tc.a, tc.b)
			if !got.IsZero() {
				t.Fatalf("chain reported priced (%+v); an unpriced member cannot be costed at the other's rates", got)
			}
		})
	}
}

func TestUnpricedChainRefusesAnEnforcingBudget(t *testing.T) {
	// The end-to-end consequence of the case above: the budget must still fail
	// closed once the intersection has run.
	caps := ModelCapabilities{Known: true, Pricing: Pricing{InputPerMTok: 3, OutputPerMTok: 15}}
	unknown := ModelCapabilities{Known: true, Pricing: Pricing{}}

	merged := caps.Intersect(unknown)
	b := NewBudget(BudgetPolicy{Limit: USD(5)})

	if _, err := b.Record("chain", merged.Pricing, TokenUsage{InputTokens: 1000, Requests: 1}); err == nil {
		t.Fatal("budget accepted usage priced from a chain containing an unpriced member")
	}
}

func TestTwoFreeMembersStayFree(t *testing.T) {
	free := Pricing{Free: true}

	got := worsePricing(free, free)
	if got.IsZero() {
		t.Fatal("free + free collapsed to unpriced; a budget would refuse a run that costs nothing")
	}
	if !got.Free {
		t.Fatalf("Free flag dropped: %+v", got)
	}

	b := NewBudget(BudgetPolicy{Limit: USD(5)})
	status, err := b.Record("local", got, TokenUsage{InputTokens: 1_000_000, Requests: 1})
	if err != nil {
		t.Fatalf("free chain rejected by an enforcing budget: %v", err)
	}
	if status != BudgetStatusOK || b.Spent() != 0 {
		t.Fatalf("free usage charged: status=%v spent=%v", status, b.Spent())
	}
}

func TestFreeAndPaidTakesThePaidRates(t *testing.T) {
	got := worsePricing(Pricing{Free: true}, Pricing{InputPerMTok: 3, OutputPerMTok: 15})
	if got.Free {
		t.Fatal("chain reported free while a paid member may serve the request")
	}
	if got.InputPerMTok != 3 || got.OutputPerMTok != 15 {
		t.Fatalf("paid rates not carried: %+v", got)
	}
}

func TestMixedCurrenciesAreNotComparable(t *testing.T) {
	usd := Pricing{Currency: DefaultCurrency, InputPerMTok: 3}
	eur := Pricing{Currency: "EUR", InputPerMTok: 4}

	got := worsePricing(usd, eur)
	if !got.IsZero() {
		t.Fatalf("rates in different currencies were merged into %+v; there is no conversion here", got)
	}
}

func TestEmptyCurrencyIsTreatedAsUSD(t *testing.T) {
	// Pricing.currency() defaults empty to USD, so an unset Currency must not
	// look like a different one and silently unprice the chain.
	got := worsePricing(Pricing{InputPerMTok: 3}, Pricing{Currency: DefaultCurrency, InputPerMTok: 4})
	if got.IsZero() {
		t.Fatal("empty Currency treated as incomparable with USD")
	}
	if got.InputPerMTok != 4 {
		t.Fatalf("expected the higher rate, got %v", got.InputPerMTok)
	}
}

func TestMergedCardKeepsTheWorseProvenance(t *testing.T) {
	a := Pricing{InputPerMTok: 3, AsOf: "2026-07-01", Source: "anthropic pricing"}
	b := Pricing{InputPerMTok: 4, AsOf: "2026-01-01", Source: "openai pricing"}

	got := worsePricing(a, b)
	if got.AsOf != "2026-01-01" {
		t.Errorf("AsOf = %q, want the older date: a merged card is only as current as its stalest rate", got.AsOf)
	}
	if got.Source != "anthropic pricing + openai pricing" {
		t.Errorf("Source = %q, want both provenances named", got.Source)
	}

	unknownDate := worsePricing(a, Pricing{InputPerMTok: 4})
	if unknownDate.AsOf != "" {
		t.Errorf("AsOf = %q, want empty when one side's date is unknown", unknownDate.AsOf)
	}
}
