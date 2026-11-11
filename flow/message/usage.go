package message

import (
	"maps"

	"github.com/urmzd/saige/flow/event"
)

// usd is the currency TreeUsage's cost is in.
const usd = "USD"

// ownTally sums one run's own model calls. A call relayed from a nested
// agent (Depth above zero) is not the run's own.
type ownTally struct {
	seen    map[string]bool
	usage   Usage
	counted bool
}

func (t *ownTally) add(c event.UsageCall) {
	if c.Depth > 0 || !first(&t.seen, c.AccountingID) {
		return
	}
	t.usage = plus(t.usage, callUsage(c))
	t.counted = true
}

// of is the run's own usage: what its calls reported, or else what the
// body that ended it says.
func (t *ownTally) of(body *event.RunUsage) *Usage {
	switch {
	case t.counted:
		u := t.usage
		return &u
	case body != nil:
		u := anthropic(body.Own)
		return &u
	}
	return &Usage{}
}

// treeTally sums every model call of the tree once, by model.
type treeTally struct {
	seen    map[string]bool
	byModel map[string]Usage
	micros  int64
	counted bool
}

func (t *treeTally) add(c event.UsageCall) {
	if !first(&t.seen, c.AccountingID) {
		return
	}
	if t.byModel == nil {
		t.byModel = map[string]Usage{}
	}
	model := firstOf(c.Model, "unknown")
	t.byModel[model] = plus(t.byModel[model], callUsage(c))
	if c.Cost != nil && c.Cost.Currency == usd {
		t.micros += c.Cost.Micros
	}
	t.counted = true
}

// of is the tree's usage: what its calls reported, or else the cost the
// root's ending body totals.
func (t *treeTally) of(body *event.RunUsage) *TreeUsage {
	by := map[string]Usage{}
	maps.Copy(by, t.byModel)
	micros := t.micros
	if !t.counted && body != nil && body.Total.Cost != nil && body.Total.Cost.Currency == usd {
		micros = body.Total.Cost.Micros
	}
	return &TreeUsage{Usage: ModelUsage{ByModel: by, CostUSD: float64(micros) / 1e6}}
}

// first reports whether id is new to seen, and marks it. An empty id is
// always new.
func first(seen *map[string]bool, id string) bool {
	if id == "" {
		return true
	}
	if (*seen)[id] {
		return false
	}
	if *seen == nil {
		*seen = map[string]bool{}
	}
	(*seen)[id] = true
	return true
}

// callUsage is one model call's tokens, Anthropic shaped: input tokens
// leave out those read from or written to the cache.
func callUsage(c event.UsageCall) Usage {
	return Usage{
		InputTokens:              max(c.PromptTokens-c.CachedPromptTokens-c.CacheWriteTokens, 0),
		OutputTokens:             c.CompletionTokens,
		CacheReadInputTokens:     c.CachedPromptTokens,
		CacheCreationInputTokens: c.CacheWriteTokens,
	}
}

func anthropic(u event.Usage) Usage {
	return Usage{
		InputTokens:              max(u.PromptTokens-u.CachedPromptTokens-u.CacheWriteTokens, 0),
		OutputTokens:             u.CompletionTokens,
		CacheReadInputTokens:     u.CachedPromptTokens,
		CacheCreationInputTokens: u.CacheWriteTokens,
	}
}

func plus(a, b Usage) Usage {
	return Usage{
		InputTokens:              a.InputTokens + b.InputTokens,
		OutputTokens:             a.OutputTokens + b.OutputTokens,
		CacheReadInputTokens:     a.CacheReadInputTokens + b.CacheReadInputTokens,
		CacheCreationInputTokens: a.CacheCreationInputTokens + b.CacheCreationInputTokens,
	}
}
