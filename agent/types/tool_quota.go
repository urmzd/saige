package types

import (
	"errors"
	"fmt"
)

// ErrToolQuotaExceeded refuses a tool call because its tool has used its
// quota of calls for the budget's lifetime. The agent loop reports it to the
// model as the call's error result, so the model can stop retrying the tool
// and continue with something else; it does not end the run.
var ErrToolQuotaExceeded = errors.New("tool quota exceeded")

// ToolQuota caps how many calls to the named tool this budget admits. A
// value of n <= 0 removes the cap. It returns b so quotas can be chained
// after NewBudget.
//
// The cap counts calls admitted by ReserveToolCall, which the agent runs
// once per call after the gate and any approval, immediately before the tool
// runs. A call admitted counts even if the tool then fails, because the
// effect a quota limits (an external request, a charge) may already have
// happened. Lowering a quota below the calls already admitted refuses every
// later call; it never undoes earlier ones.
//
// Sub-agents that share the budget share the quota, so a child cannot reset
// its parent's allowance for a tool.
func (b *Budget) ToolQuota(name string, n int) *Budget {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n <= 0 {
		delete(b.toolQuotas, name)
		return b
	}
	if b.toolQuotas == nil {
		b.toolQuotas = map[string]int{}
	}
	b.toolQuotas[name] = n
	return b
}

// ReserveToolCall admits one call to the named tool, or returns an error
// wrapping ErrToolQuotaExceeded when its quota is used up. Tools without a
// quota are always admitted and still counted, so ToolCalls reports usage.
// Safe for concurrent use: concurrent calls never admit more than the quota.
func (b *Budget) ReserveToolCall(name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if limit, ok := b.toolQuotas[name]; ok && b.toolCalls[name] >= limit {
		return fmt.Errorf("%w: %s allows %d calls", ErrToolQuotaExceeded, name, limit)
	}
	if b.toolCalls == nil {
		b.toolCalls = map[string]int{}
	}
	b.toolCalls[name]++
	return nil
}

// ToolCalls returns how many calls to the named tool have been admitted.
func (b *Budget) ToolCalls(name string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.toolCalls[name]
}

// ToolQuotaRemaining returns the calls left for the named tool and whether a
// quota is set for it.
func (b *Budget) ToolQuotaRemaining(name string) (int, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	limit, ok := b.toolQuotas[name]
	if !ok {
		return 0, false
	}
	return max(limit-b.toolCalls[name], 0), true
}
