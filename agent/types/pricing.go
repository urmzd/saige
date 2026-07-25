package types

import (
	"fmt"
	"strings"
)

// Cost is an amount of money in micro-units of its currency: 1_000_000 Cost is
// one dollar when the currency is USD.
//
// Integer micro-units rather than float dollars because costs are accumulated
// across thousands of calls and compared against a hard limit. Float addition
// drifts, and a budget that stops at 99.9997% of the limit because of rounding
// is a budget that does not stop.
type Cost int64

// Micro is one micro-unit; Cent and Unit are the obvious multiples.
const (
	Micro Cost = 1
	Cent  Cost = 10_000
	Unit  Cost = 1_000_000
)

// USD converts a float amount to Cost, rounding to the nearest micro-unit.
// Use it for limits, which humans write as "5.00", not for accumulating usage.
func USD(amount float64) Cost {
	if amount >= 0 {
		return Cost(amount*1e6 + 0.5)
	}
	return Cost(amount*1e6 - 0.5)
}

// Float returns the cost as a fractional currency amount, for display only.
func (c Cost) Float() float64 { return float64(c) / 1e6 }

// String renders the cost with enough precision to be useful at small
// magnitudes, where per-call costs actually live.
func (c Cost) String() string {
	if c == 0 {
		return "0"
	}
	if c < Cent && c > -Cent {
		return fmt.Sprintf("%.6f", c.Float())
	}
	return fmt.Sprintf("%.4f", c.Float())
}

// Pricing is what one model charges. Rates are per million tokens, matching how
// every vendor publishes them, so a row can be copied from a pricing page
// without unit conversion.
//
// Prices change without notice and differ by region, tier, and contract, so
// AsOf and Source are not decoration: a budget enforced against a stale rate
// card silently under-counts. A zero Pricing means unpriced, and a Budget
// treats unpriced usage as a hard error rather than as free.
type Pricing struct {
	// Currency is an ISO code. Empty is treated as "USD".
	Currency string
	// InputPerMTok is the rate for uncached prompt tokens.
	InputPerMTok float64
	// OutputPerMTok is the rate for generated tokens. Reasoning tokens are
	// billed at this rate by every provider that bills them separately from
	// visible output.
	OutputPerMTok float64
	// CachedInputPerMTok is the discounted rate for a prompt-cache read. Zero
	// means "same as input", not "free".
	CachedInputPerMTok float64
	// CacheWritePerMTok is the premium rate for populating a prompt cache. Zero
	// means "same as input".
	CacheWritePerMTok float64
	// PerRequest is a flat charge per call, used by server-side tools such as
	// web search that bill per invocation rather than per token.
	PerRequest float64
	// AsOf is the date these rates were recorded, e.g. "2026-07-24". Stale
	// rates are the most likely reason a budget disagrees with an invoice.
	AsOf string
	// Source names where the numbers came from.
	Source string
	// Free marks a model that genuinely costs nothing to call, such as one
	// running locally under ollama. Without it a local model is
	// indistinguishable from an unpriced remote one, and a budget cannot tell
	// "this is free" from "I do not know what this costs".
	Free bool
}

// IsZero reports whether any rate is set. An all-zero Pricing means the model
// is unpriced, which is different from being free.
func (p Pricing) IsZero() bool {
	if p.Free {
		return false // priced, at zero
	}
	return p.InputPerMTok == 0 && p.OutputPerMTok == 0 &&
		p.CachedInputPerMTok == 0 && p.CacheWritePerMTok == 0 && p.PerRequest == 0
}

func (p Pricing) currency() string {
	if p.Currency == "" {
		return "USD"
	}
	return p.Currency
}

// TokenUsage is the token breakdown a cost is computed from. It is separate
// from UsageDelta because a delta is a streaming increment while this is a
// settled total, and because the cache fields have no delta representation yet
// (see UsageDelta).
type TokenUsage struct {
	// InputTokens are uncached prompt tokens.
	InputTokens int
	// CachedInputTokens were served from a prompt cache.
	CachedInputTokens int
	// CacheWriteTokens were written into a prompt cache.
	CacheWriteTokens int
	// OutputTokens are generated tokens, reasoning included.
	OutputTokens int
	// Requests is the number of billable calls, for per-request charges.
	Requests int
}

// Add accumulates another usage into this one.
func (u *TokenUsage) Add(other TokenUsage) {
	u.InputTokens += other.InputTokens
	u.CachedInputTokens += other.CachedInputTokens
	u.CacheWriteTokens += other.CacheWriteTokens
	u.OutputTokens += other.OutputTokens
	u.Requests += other.Requests
}

// Total returns every token counted, cached and uncached.
func (u TokenUsage) Total() int {
	return u.InputTokens + u.CachedInputTokens + u.CacheWriteTokens + u.OutputTokens
}

// UsageFromDelta converts a streamed UsageDelta into a settled TokenUsage,
// counting one request. Cache fields stay zero: no adapter reports them yet, so
// cached reads are currently priced at the full input rate, which over-counts
// rather than under-counts. Over-counting is the safe direction for a budget.
func UsageFromDelta(d UsageDelta) TokenUsage {
	return TokenUsage{
		InputTokens:  d.PromptTokens,
		OutputTokens: d.CompletionTokens,
		Requests:     1,
	}
}

// Cost computes what the usage costs at these rates. Rates left zero fall back
// to the input rate for the cache tiers, since "unset" there means "not
// discounted", never "free".
func (p Pricing) Cost(u TokenUsage) Cost {
	if p.Free {
		return 0
	}
	cachedRate := p.CachedInputPerMTok
	if cachedRate == 0 {
		cachedRate = p.InputPerMTok
	}
	writeRate := p.CacheWritePerMTok
	if writeRate == 0 {
		writeRate = p.InputPerMTok
	}
	total := perMTok(u.InputTokens, p.InputPerMTok) +
		perMTok(u.CachedInputTokens, cachedRate) +
		perMTok(u.CacheWriteTokens, writeRate) +
		perMTok(u.OutputTokens, p.OutputPerMTok) +
		float64(u.Requests)*p.PerRequest
	return USD(total)
}

func perMTok(tokens int, ratePerM float64) float64 {
	return float64(tokens) / 1e6 * ratePerM
}

// Describe renders the rate card for a CLI listing.
func (p Pricing) Describe() string {
	if p.Free {
		return "free (local execution)"
	}
	if p.IsZero() {
		return "unpriced"
	}
	var parts []string
	parts = append(parts, fmt.Sprintf("in %.2f/Mtok", p.InputPerMTok))
	parts = append(parts, fmt.Sprintf("out %.2f/Mtok", p.OutputPerMTok))
	if p.CachedInputPerMTok > 0 {
		parts = append(parts, fmt.Sprintf("cached-in %.2f/Mtok", p.CachedInputPerMTok))
	}
	if p.PerRequest > 0 {
		parts = append(parts, fmt.Sprintf("%.4f/request", p.PerRequest))
	}
	s := p.currency() + " " + strings.Join(parts, ", ")
	if p.AsOf != "" {
		s += " (as of " + p.AsOf + ")"
	}
	return s
}
