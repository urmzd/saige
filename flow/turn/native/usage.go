package native

import (
	"strconv"
	"strings"

	saigetypes "github.com/urmzd/saige/agent/types"

	"github.com/urmzd/saige/flow/event"
)

// DefaultAccounting is the prefix a turn keys its calls under when the caller
// gave none (Turn.Accounting): unique within the turn, and nowhere else.
const DefaultAccounting = "turn"

// accountant turns saige's usage reports into run.usage.v1 bodies for one turn.
//
// What it knows about the model comes from saige: the rate card and the
// context window from the provider's declared capabilities, which saige's
// adapters resolve from its model catalog. A provider that declares nothing
// (an unknown model, a test double) is reported with its tokens and no cost
// or window, never with a guess.
//
// An agent with fallback models has more than one rate card, and a call is
// priced on the card of the model that answered it, found by the model name
// the response reports. A call that cannot be attributed to exactly one model
// is reported with its tokens and no cost, for the same reason.
type accountant struct {
	prefix string
	agent  string
	model  string
	caps   saigetypes.ModelCapabilities
	// members are the models of a fallback chain, primary first; empty for
	// an agent with one model.
	members []memberCaps
	calls   int
}

// memberCaps is one model of a fallback chain and what saige declares for it.
type memberCaps struct {
	names []string
	caps  saigetypes.ModelCapabilities
}

func newAccountant(prefix, agent, model string, provider saigetypes.Provider, models ...saigetypes.Provider) *accountant {
	if prefix == "" {
		prefix = DefaultAccounting
	}
	caps, _ := saigetypes.ProviderCapabilities(provider)
	// The provider's own name for its model first, then the catalog entry it
	// resolved to, then the name the pinned revision gave.
	model = firstNonEmpty(saigetypes.ProviderModel(provider), caps.Model, model)
	a := &accountant{prefix: prefix, agent: agent, model: model, caps: caps}
	if len(models) > 1 {
		for _, m := range models {
			mc, _ := saigetypes.ProviderCapabilities(m)
			var names []string
			for _, n := range []string{saigetypes.ProviderModel(m), mc.Model} {
				if n != "" {
					names = append(names, n)
				}
			}
			a.members = append(a.members, memberCaps{names: names, caps: mc})
		}
	}
	return a
}

// capsFor returns what to price a call on, and whether it may be priced at
// all. One model is its own answer. In a chain, the member whose model the
// response names: exactly, or as the dated snapshot of an alias
// ("claude-x-20260101" for "claude-x"), the longest such name winning. Two
// members that match equally (one model behind two providers) cannot be told
// apart from the response, so such a call is not priced.
func (a *accountant) capsFor(responseModel string) (saigetypes.ModelCapabilities, bool) {
	if len(a.members) == 0 {
		return a.caps, true
	}
	best, found := -1, []int(nil)
	for i, m := range a.members {
		for _, n := range m.names {
			if responseModel != n && !strings.HasPrefix(responseModel, n+"-") {
				continue
			}
			switch {
			case len(n) > best:
				best, found = len(n), []int{i}
			case len(n) == best && found[len(found)-1] != i:
				found = append(found, i)
			}
		}
	}
	if len(found) != 1 {
		return saigetypes.ModelCapabilities{}, false
	}
	return a.members[found[0]].caps, true
}

// call reports one model call. saige reports usage once per completed provider
// request, already merged, so each delta is one call.
//
// A response saige served from its response cache repeats the counts of the
// call it replayed. Those tokens were not spent again: the call is reported
// with zero tokens, zero requests and no cost, and flagged, which is how saige
// itself settles it.
func (a *accountant) call(d saigetypes.UsageDelta) event.UsageCall {
	a.calls++
	c := event.UsageCall{
		// Always this turn's own key, never saige's: saige keeps its id
		// stable across a durable replay of its own, so two attempts' real
		// calls could share it and be counted once.
		AccountingID:      a.prefix + "/" + strconv.Itoa(a.calls),
		SaigeAccountingID: d.AccountingID,
		Agent:             a.agent,
		Model:             firstNonEmpty(d.ResponseModel, a.model),
	}
	caps, priced := a.capsFor(d.ResponseModel)
	c.ContextWindow = caps.ContextWindow
	if d.CacheHit {
		c.ResponseCacheHit = true
		return c
	}
	billed := saigetypes.UsageFromDelta(d)
	c.Requests = billed.Requests
	c.PromptTokens = d.PromptTokens
	c.CachedPromptTokens = d.CachedPromptTokens
	c.CacheWriteTokens = d.CacheWriteTokens
	c.CompletionTokens = d.CompletionTokens
	if billed.Total() == 0 {
		// The provider reported no tokens for a call it served: saige
		// still reports the call, and pricing it would claim it cost
		// nothing. It is counted, with no cost.
		return c
	}
	if p := caps.Pricing; priced && !p.IsZero() && !p.Unpriced(billed) {
		currency := p.Currency
		if currency == "" {
			currency = saigetypes.DefaultCurrency
		}
		c.Cost = &event.Cost{Micros: int64(p.Cost(billed)), Currency: currency, AsOf: p.AsOf}
	}
	return c
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
