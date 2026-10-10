package types

import (
	"reflect"
	"testing"
)

func TestPricingBatch(t *testing.T) {
	p := Pricing{InputPerMTok: 2, OutputPerMTok: 10, CachedInputPerMTok: 0.2, CacheWritePerMTok: 2.5,
		PerRequest: 0.01, BatchDiscount: 0.5, Source: "list"}
	b := p.Batch()
	if b.InputPerMTok != 1 || b.OutputPerMTok != 5 || b.CacheWritePerMTok != 1.25 || b.PerRequest != 0.01 {
		t.Fatalf("batch = %+v", b)
	}
	// Without a declared batch cache rate, a cache read keeps its rate:
	// vendors differ on whether the discounts stack.
	if b.CachedInputPerMTok != 0.2 {
		t.Fatalf("cached = %v, want 0.2", b.CachedInputPerMTok)
	}
	if b.BatchDiscount != 0 || !reflect.DeepEqual(b.Batch(), b) {
		t.Fatal("Batch applied twice discounts twice")
	}
	p.BatchCachedInputPerMTok = 0.1
	if got := p.Batch().CachedInputPerMTok; got != 0.1 {
		t.Fatalf("declared batch cache rate = %v", got)
	}
	// No declared discount: the interactive rates, an over-count.
	if got := (Pricing{InputPerMTok: 2}).Batch(); got.InputPerMTok != 2 {
		t.Fatalf("undiscounted = %+v", got)
	}
	u := TokenUsage{InputTokens: 1_000_000, OutputTokens: 1_000_000, Requests: 1}
	if got, want := p.Batch().Cost(u), USD(6.01); got != want {
		t.Fatalf("cost = %s, want %s", got, want)
	}
	if free := (Pricing{Free: true, BatchDiscount: 0.5}).Batch(); !free.Free || free.Cost(u) != 0 {
		t.Fatalf("free = %+v", free)
	}
}

func TestWorsePricingKeepsSmallerBatchDiscount(t *testing.T) {
	a := Pricing{InputPerMTok: 1, BatchDiscount: 0.5, BatchCachedInputPerMTok: 0.05}
	b := Pricing{InputPerMTok: 1, BatchDiscount: 0.25}
	got := worsePricing(a, b)
	if got.BatchDiscount != 0.25 || got.BatchCachedInputPerMTok != 0 {
		t.Fatalf("merged = %+v", got)
	}
}
