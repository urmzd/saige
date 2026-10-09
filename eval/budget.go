package eval

import (
	"context"
	"fmt"
	"time"
)

// Budget scorers check an observation's recorded usage against a limit:
// tokens, cost, and wall-clock time. Like the check scorers, each scores 1
// within budget and 0 over it, with a reason giving both numbers, and is
// marked [Deterministic]. They decline an observation whose subject did not
// record the measure, so an unrecorded cost is never reported as free.

// TokenBudgetScorer checks that input plus output tokens stay at or below
// maxTokens. It declines observations with no recorded tokens. The metric is
// "token_budget".
func TokenBudgetScorer(maxTokens int) Scorer {
	const name = "token_budget"
	return NewCheckScorer(name, func(_ context.Context, obs Observation) (bool, string, error) {
		used, ok := obs.Timing.TotalTokens()
		if !ok {
			return false, "", ErrNotApplicable
		}
		return used <= maxTokens, fmt.Sprintf("used %d tokens, budget %d", used, maxTokens), nil
	})
}

// CostBudgetScorer checks that the observation's cost stays at or below
// maxUSD. It declines observations with no recorded cost
// ([ObservationTiming.CostUSD] is nil). The metric is "cost_budget".
func CostBudgetScorer(maxUSD float64) Scorer {
	const name = "cost_budget"
	return NewCheckScorer(name, func(_ context.Context, obs Observation) (bool, string, error) {
		if obs.Timing.CostUSD == nil {
			return false, "", ErrNotApplicable
		}
		cost := *obs.Timing.CostUSD
		return cost <= maxUSD, fmt.Sprintf("cost $%.6f, budget $%.6f", cost, maxUSD), nil
	})
}

// RespondsWithinScorer checks that the observation's total time stays at or
// below limit. It declines observations with no recorded time (TotalMs is
// zero). The metric is "responds_within".
func RespondsWithinScorer(limit time.Duration) Scorer {
	const name = "responds_within"
	return NewCheckScorer(name, func(_ context.Context, obs Observation) (bool, string, error) {
		if obs.Timing.TotalMs <= 0 {
			return false, "", ErrNotApplicable
		}
		took := time.Duration(obs.Timing.TotalMs) * time.Millisecond
		return took <= limit, fmt.Sprintf("took %s, limit %s", took, limit), nil
	})
}

// CostScorer reports the observation's cost in USD as the metric
// "cost_usd". It declines observations with no recorded cost.
func CostScorer() Scorer {
	const name = "cost_usd"
	return Deterministic(NewScorerFunc(name, func(_ context.Context, obs Observation) (Score, error) {
		if obs.Timing.CostUSD == nil {
			return Score{}, nil
		}
		return Score{Name: name, Value: *obs.Timing.CostUSD}, nil
	}))
}

// TotalTokensScorer reports input plus output tokens as the metric
// "total_tokens". It declines observations with no recorded tokens.
func TotalTokensScorer() Scorer {
	const name = "total_tokens"
	return Deterministic(NewScorerFunc(name, func(_ context.Context, obs Observation) (Score, error) {
		used, ok := obs.Timing.TotalTokens()
		if !ok {
			return Score{}, nil
		}
		return Score{Name: name, Value: float64(used)}, nil
	}))
}

// TotalTokens returns input plus output tokens. ok is false when neither was
// recorded.
func (t ObservationTiming) TotalTokens() (total int, ok bool) {
	if t.InputTokens == 0 && t.OutputTokens == 0 {
		return 0, false
	}
	return t.InputTokens + t.OutputTokens, true
}

// SetCostUSD records the observation's cost in USD.
func (t *ObservationTiming) SetCostUSD(usd float64) {
	t.CostUSD = &usd
}
