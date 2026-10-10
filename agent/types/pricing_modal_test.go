package types

import (
	"errors"
	"strings"
	"testing"
)

func modalCard() Pricing {
	return Pricing{InputPerMTok: 1, OutputPerMTok: 4, Modal: map[Modality]ModalityRate{
		ModalityAudio: {InputPerMTok: 3, OutputPerMTok: 12},
		ModalityImage: {InputPerMTok: 1},
	}}
}

// TestModalCostBillsEachModalityAtItsRate checks that priced modality
// tokens are billed at their own rates out of the text counts, and that
// text tokens reported by modality stay at the text rates.
func TestModalCostBillsEachModalityAtItsRate(t *testing.T) {
	u := UsageFromDelta(UsageDelta{PromptTokens: 3_000_000, CompletionTokens: 2_000_000,
		PromptByModality:     map[Modality]int{ModalityText: 1_000_000, ModalityAudio: 1_000_000, ModalityImage: 1_000_000},
		CompletionByModality: map[Modality]int{ModalityText: 1_000_000, ModalityAudio: 1_000_000}})
	// text 1M x 1 + audio 1M x 3 + image 1M x 1, out text 1M x 4 + audio 1M x 12
	if got, want := modalCard().Cost(u), USD(1+3+1+4+12); got != want {
		t.Fatalf("cost = %s, want %s", got, want)
	}
	if modalCard().Unpriced(u) {
		t.Fatal("every modality is priced")
	}
	// Modality counts above the uncached input are clamped, never negative.
	cached := TokenUsage{CachedInputTokens: 1_000_000, InputByModality: map[Modality]int{ModalityAudio: 1_000_000}}
	if got := modalCard().Cost(cached); got != USD(1) {
		t.Fatalf("cost with only cached input = %s, want the cache read at the input rate", got)
	}
}

// TestModalityWithoutRateIsUnpriced checks the unpriced-call rule: usage
// of a modality the card has no rate for, in that direction, cannot be
// costed, while a card without modal rates and text-only usage can.
func TestModalityWithoutRateIsUnpriced(t *testing.T) {
	for _, tc := range []struct {
		name     string
		pricing  Pricing
		usage    TokenUsage
		unpriced bool
	}{
		{"video input without a rate", modalCard(), TokenUsage{InputTokens: 10, InputByModality: map[Modality]int{ModalityVideo: 5}}, true},
		{"image output without a rate", modalCard(), TokenUsage{OutputTokens: 10, OutputByModality: map[Modality]int{ModalityImage: 5}}, true},
		{"text by modality", Pricing{InputPerMTok: 1}, TokenUsage{InputTokens: 10, InputByModality: map[Modality]int{ModalityText: 10}}, false},
		{"zero count", Pricing{InputPerMTok: 1}, TokenUsage{InputTokens: 10, InputByModality: map[Modality]int{ModalityAudio: 0}}, false},
		{"free", Pricing{Free: true}, TokenUsage{InputTokens: 10, InputByModality: map[Modality]int{ModalityAudio: 10}}, false},
		{"no card", Pricing{}, TokenUsage{InputTokens: 10}, true},
	} {
		if got := tc.pricing.Unpriced(tc.usage); got != tc.unpriced {
			t.Errorf("%s: Unpriced = %v, want %v", tc.name, got, tc.unpriced)
		}
	}
}

// TestSettleUnpricedModalityFailsClosed checks that settling usage of an
// unpriced modality records it at the text rates as uncertain and returns
// ErrUnpriced under an enforcing policy, and only records it when the
// policy allows unpriced calls.
func TestSettleUnpricedModalityFailsClosed(t *testing.T) {
	usage := TokenUsage{InputTokens: 1_000_000, InputByModality: map[Modality]int{ModalityVideo: 500_000}, Requests: 1}
	for _, allow := range []bool{false, true} {
		b := NewBudget(BudgetPolicy{Limit: USD(10), AllowUnpriced: allow})
		r, err := b.ReserveWith("r1", modalCard(), ConversionEstimate{})
		if err != nil {
			t.Fatal(err)
		}
		err = b.Settle(r.ID, "m", modalCard(), usage, false)
		if allow != (err == nil) || (!allow && !errors.Is(err, ErrUnpriced)) {
			t.Fatalf("allow %v: err = %v", allow, err)
		}
		if !allow && !strings.Contains(err.Error(), "video input") {
			t.Errorf("err = %v, want it to name the modality", err)
		}
		if rc := b.Receipt(r.ID); !rc.Uncertain || rc.Cost != USD(1) {
			t.Errorf("allow %v: receipt = %+v, want the text-rate lower bound, uncertain", allow, rc)
		}
		if b.Uncertain() != 1 {
			t.Errorf("allow %v: uncertain = %d", allow, b.Uncertain())
		}
		if _, err := b.RecordOnce(r.ID, "m", modalCard(), usage); allow != (err == nil) {
			t.Errorf("allow %v: RecordOnce err = %v", allow, err)
		}
	}
}

// TestModalRatesFollowTiersAndIntersection checks that the batch and
// discounted tiers discount modal rates, that an offering projects its
// modality pricing onto its rate card, and that intersecting two cards
// keeps only modalities both price, at the costlier rate.
func TestModalRatesFollowTiersAndIntersection(t *testing.T) {
	p := modalCard()
	p.BatchDiscount = 0.5
	if got := p.Batch().Modal[ModalityAudio]; got != (ModalityRate{InputPerMTok: 1.5, OutputPerMTok: 6}) {
		t.Fatalf("batch audio = %+v", got)
	}
	o := Offering{Pricing: Pricing{InputPerMTok: 1, OutputPerMTok: 4},
		ModalityPricing: map[Modality]ModalityRate{ModalityAudio: {InputPerMTok: 3}},
		Tiers:           map[ServiceTier]TierSpec{ServiceFlex: {Discount: 0.5}}}
	if got := o.Capabilities().Pricing.Modal[ModalityAudio].InputPerMTok; got != 3 {
		t.Fatalf("projected audio rate = %v", got)
	}
	if tp, _ := o.TierPricing(ServiceFlex); tp.Modal[ModalityAudio].InputPerMTok != 1.5 {
		t.Fatalf("flex audio rate = %+v", tp.Modal)
	}
	back := OfferingFromCapabilities(o.Capabilities())
	if back.Pricing.Modal != nil || back.ModalityPricing[ModalityAudio].InputPerMTok != 3 {
		t.Fatalf("round trip = %+v / %+v", back.Pricing, back.ModalityPricing)
	}
	other := Pricing{InputPerMTok: 2, OutputPerMTok: 4, Modal: map[Modality]ModalityRate{ModalityAudio: {InputPerMTok: 5}}}
	w := worsePricing(modalCard(), other)
	if len(w.Modal) != 1 || w.Modal[ModalityAudio] != (ModalityRate{InputPerMTok: 5}) {
		t.Fatalf("intersected modal = %+v", w.Modal)
	}
}
