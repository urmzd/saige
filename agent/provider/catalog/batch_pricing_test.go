package catalog

import (
	"strings"
	"testing"
)

func TestBatchDiscountValidated(t *testing.T) {
	doc := `{"version":1,"models":[{"provider":"openai","prefix":"x-test","pricing":{"input_per_mtok":1,"batch_discount":1.5,"as_of":"2026-10-09"}}]}`
	c, err := Load(strings.NewReader(doc))
	if err == nil {
		err = c.Validate()
	}
	issueAt(t, err, "models[0].pricing.batch_discount", CodePricing)
}

// TestDefaultCatalogBatchPricing checks the declared batch discounts: half
// price on the Anthropic, OpenAI and Gemini batch APIs, with cache reads
// discounted again only where the vendor stacks the discounts.
func TestDefaultCatalogBatchPricing(t *testing.T) {
	for _, tc := range []struct {
		provider, model string
		stacked         bool
	}{
		{"anthropic", "claude-haiku-5-5", true},
		{"openai", "gpt-6-luna", true},
		{"google", "gemini-3.1-flash-lite", false},
	} {
		p := MustLookup(tc.provider, tc.model).Pricing
		if p.BatchDiscount != 0.5 {
			t.Fatalf("%s batch discount = %v, want 0.5", tc.model, p.BatchDiscount)
		}
		b := p.Batch()
		if b.InputPerMTok != p.InputPerMTok/2 || b.OutputPerMTok != p.OutputPerMTok/2 {
			t.Fatalf("%s batch rates = %+v", tc.model, b)
		}
		if stacked := p.BatchCachedInputPerMTok > 0; stacked != tc.stacked {
			t.Fatalf("%s stacked cache discount = %v, want %v", tc.model, stacked, tc.stacked)
		}
	}
}
