package event

import (
	"encoding/json"
	"slices"

	"github.com/urmzd/saige/flow/address"
)

// TargetAddress parses the address the action was taken on. An action with
// no target returns the zero address.
func (a Action) TargetAddress() (address.Address, error) { return parseAddr(a.Target) }

// ResultAddress parses the address of what the action produced. An action
// with none returns the zero address.
func (a Action) ResultAddress() (address.Address, error) { return parseAddr(a.Address) }

func parseAddr(s string) (address.Address, error) {
	if s == "" {
		return address.Address{}, nil
	}
	return address.Parse(s)
}

// ChildRunAccountingID is the key a child run's usage is counted under on its
// parent's stream.
func ChildRunAccountingID(runID string) string { return "run:" + runID }

// Add returns u with o added. Money adds only within one currency: a cost in
// another currency is summed in OtherCosts, never converted.
func (u Usage) Add(o Usage) Usage {
	u.ModelCalls += o.ModelCalls
	u.Requests += o.Requests
	u.PromptTokens += o.PromptTokens
	u.CachedPromptTokens += o.CachedPromptTokens
	u.CacheWriteTokens += o.CacheWriteTokens
	u.CompletionTokens += o.CompletionTokens
	u.UnpricedCalls += o.UnpricedCalls
	if o.Cost != nil {
		u = u.addCost(*o.Cost)
	}
	for _, c := range o.OtherCosts {
		u = u.addCost(c)
	}
	return u
}

// AddCall returns u with one model call added. A call with no price that was
// not a response cache hit counts as unpriced.
func (u Usage) AddCall(c UsageCall) Usage {
	u.ModelCalls++
	u.Requests += c.Requests
	u.PromptTokens += c.PromptTokens
	u.CachedPromptTokens += c.CachedPromptTokens
	u.CacheWriteTokens += c.CacheWriteTokens
	u.CompletionTokens += c.CompletionTokens
	switch {
	case c.Cost != nil:
		u = u.addCost(*c.Cost)
	case !c.ResponseCacheHit:
		u.UnpricedCalls++
	}
	return u
}

func (u Usage) addCost(c Cost) Usage {
	plus := func(sum Cost) Cost {
		sum.Micros += c.Micros
		sum.AsOf = olderAsOf(sum.AsOf, c.AsOf)
		return sum
	}
	switch {
	case u.Cost == nil:
		out := c
		u.Cost = &out
		return u
	case u.Cost.Currency == c.Currency:
		out := plus(*u.Cost)
		u.Cost = &out
		return u
	}
	others := append([]Cost(nil), u.OtherCosts...)
	for i := range others {
		if others[i].Currency == c.Currency {
			others[i] = plus(others[i])
			u.OtherCosts = others
			return u
		}
	}
	u.OtherCosts = append(others, c)
	return u
}

// olderAsOf is the older of two price card dates. A total priced from a card
// with no date has no date.
func olderAsOf(a, b string) string {
	if a == "" || b == "" {
		return ""
	}
	return min(a, b)
}

// CacheReadRatio is the share of prompt tokens read from the cache.
func (u Usage) CacheReadRatio() float64 {
	if u.PromptTokens <= 0 {
		return 0
	}
	return float64(u.CachedPromptTokens) / float64(u.PromptTokens)
}

// Tally sums usage reports, counting each AccountingID once. Delivery is at
// least once and a call is relayed up every level of a delegation, so the
// same report can arrive more than once. A zero Tally is ready to use.
type Tally struct {
	seen     map[string]bool
	own      Usage
	children Usage
	context  *ContextUsage
}

// Call counts one model call and reports whether it was new.
func (t *Tally) Call(c UsageCall) bool {
	if !t.first(c.AccountingID) {
		return false
	}
	if c.Depth > 0 {
		t.children = t.children.AddCall(c)
		return true
	}
	t.own = t.own.AddCall(c)
	if !c.ResponseCacheHit {
		t.context = &ContextUsage{Model: c.Model, Window: c.ContextWindow, Tokens: c.PromptTokens}
	}
	return true
}

// Child counts a child run's whole usage and reports whether it was new.
func (t *Tally) Child(c UsageChild) bool {
	if !t.first(c.AccountingID) {
		return false
	}
	t.children = t.children.Add(c.Usage)
	return true
}

func (t *Tally) first(id string) bool {
	if id == "" {
		return true
	}
	if t.seen[id] {
		return false
	}
	if t.seen == nil {
		t.seen = make(map[string]bool)
	}
	t.seen[id] = true
	return true
}

// Usage is what was counted, split by who spent it.
func (t *Tally) Usage() RunUsage {
	out := RunUsage{Own: t.own, Children: t.children, Total: t.own.Add(t.children)}
	if t.context != nil {
		c := *t.context
		out.Context = &c
	}
	return out
}

// tallyJSON is a Tally as it is stored: what it has counted and the
// accounting ids it has seen, so a tally read back counts a redelivered
// report once, whichever process reads it.
type tallyJSON struct {
	Seen     []string      `json:"seen,omitempty"`
	Own      Usage         `json:"own"`
	Children Usage         `json:"children"`
	Context  *ContextUsage `json:"context,omitempty"`
}

// MarshalJSON keeps a tally, its seen accounting ids sorted.
func (t Tally) MarshalJSON() ([]byte, error) {
	seen := make([]string, 0, len(t.seen))
	for id := range t.seen {
		seen = append(seen, id)
	}
	slices.Sort(seen)
	return json.Marshal(tallyJSON{Seen: seen, Own: t.own, Children: t.children, Context: t.context})
}

// UnmarshalJSON reads a tally MarshalJSON kept.
func (t *Tally) UnmarshalJSON(b []byte) error {
	var v tallyJSON
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*t = Tally{own: v.Own, children: v.Children, context: v.Context}
	for _, id := range v.Seen {
		t.first(id)
	}
	return nil
}
