package types

import (
	"errors"
	"fmt"
	"sync"
)

// ErrBudgetExceeded ends a run that has spent its allowance. It is returned
// rather than logged: a run that quietly continues past its ceiling is the
// failure the budget exists to prevent.
var ErrBudgetExceeded = errors.New("budget exceeded")

// ErrUnpriced ends a run that cannot be costed. A model with no rate card
// cannot be held to a spending limit, and treating unknown as free is how an
// unpriced model becomes the expensive one.
var ErrUnpriced = errors.New("model has no pricing; cannot enforce a budget")

// BudgetAction is what happens when a limit is reached.
type BudgetAction int

const (
	// BudgetStop ends the run with ErrBudgetExceeded. The default, because it
	// is the only action that needs no one to be watching.
	BudgetStop BudgetAction = iota
	// BudgetRequireApproval pauses and asks a human whether to continue. The
	// run resumes with the limit raised by BudgetPolicy.ApprovalGrant, so
	// approving once does not disable the budget for the rest of the run.
	BudgetRequireApproval
	// BudgetWarn records the breach and keeps going. For observability only:
	// this does not enforce anything.
	BudgetWarn
)

// BudgetPolicy is the spending ceiling for a run and what to do at it.
//
// Enforcement is checked on every usage report rather than once per iteration.
// A single call to a long-context model can cost more than the whole budget, so
// a per-iteration check can overshoot by an unbounded amount; a per-usage check
// overshoots by at most one call.
type BudgetPolicy struct {
	// Limit is the hard ceiling. Zero means unlimited, and every other field is
	// then inert.
	Limit Cost
	// WarnAt is the fraction of Limit (0 to 1) at which Status starts reporting
	// BudgetStatusWarn. Zero disables the warning tier.
	WarnAt float64
	// OnExceed is what happens at the limit.
	OnExceed BudgetAction
	// ApprovalGrant is how much additional allowance a human approval buys when
	// OnExceed is BudgetRequireApproval. Zero defaults to the original Limit,
	// so approving doubles the allowance rather than removing it.
	ApprovalGrant Cost
	// MaxRequests caps billable provider calls regardless of cost. Zero means
	// unlimited. It is the backstop for unpriced or mispriced models.
	MaxRequests int
	// MaxTokens caps total tokens regardless of cost. Zero means unlimited.
	MaxTokens int
	// AllowUnpriced permits a run against a model with no rate card. The tokens
	// and requests are still counted against MaxTokens and MaxRequests; only
	// the cost ceiling is unenforceable. Defaults false: fail closed.
	AllowUnpriced bool
}

// BudgetStatus is the outcome of a check.
type BudgetStatus int

const (
	// BudgetStatusOK: under every limit.
	BudgetStatusOK BudgetStatus = iota
	// BudgetStatusWarn: past WarnAt but under the limit.
	BudgetStatusWarn
	// BudgetStatusExceeded: at or past a limit.
	BudgetStatusExceeded
)

func (s BudgetStatus) String() string {
	switch s {
	case BudgetStatusWarn:
		return "warn"
	case BudgetStatusExceeded:
		return "exceeded"
	default:
		return "ok"
	}
}

// Budget tracks spend against a policy. Safe for concurrent use: sub-agents and
// parallel tool calls report into the same budget, and a budget that each
// branch tracked separately would enforce nothing at the top.
//
// Share one Budget across an agent and its sub-agents to cap a whole run; give
// a sub-agent its own to cap that delegation independently.
type Budget struct {
	policy BudgetPolicy

	mu       sync.Mutex
	spent    Cost
	usage    TokenUsage
	byModel  map[string]TokenUsage
	costs    map[string]Cost
	currency map[string]string // per model, so a Report can name its unit
	granted  Cost              // extra allowance bought by approvals
	breaches int
}

// NewBudget returns a budget enforcing the policy. A zero policy tracks spend
// without limiting it, which is still useful for reporting.
func NewBudget(policy BudgetPolicy) *Budget {
	return &Budget{
		policy:   policy,
		byModel:  map[string]TokenUsage{},
		costs:    map[string]Cost{},
		currency: map[string]string{},
	}
}

// Policy returns the enforced policy.
func (b *Budget) Policy() BudgetPolicy { return b.policy }

// Record adds usage for a model and returns the status after adding it.
//
// An unpriced model returns ErrUnpriced unless the policy allows it. Usage is
// still recorded in that case, so a report shows what was spent even where it
// could not be costed.
func (b *Budget) Record(model string, pricing Pricing, u TokenUsage) (BudgetStatus, error) {
	cost := pricing.Cost(u)
	unpriced := pricing.IsZero() && u.Total() > 0

	b.mu.Lock()
	b.usage.Add(u)
	existing := b.byModel[model]
	existing.Add(u)
	b.byModel[model] = existing
	b.spent += cost
	b.costs[model] += cost
	if !pricing.IsZero() {
		b.currency[model] = pricing.currency()
	}
	status := b.statusLocked()
	if status == BudgetStatusExceeded {
		b.breaches++
	}
	b.mu.Unlock()

	if unpriced && !b.policy.AllowUnpriced && b.policy.Limit > 0 {
		return status, fmt.Errorf("%w: %s", ErrUnpriced, model)
	}
	return status, nil
}

// Status reports the current tier without recording anything.
func (b *Budget) Status() BudgetStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.statusLocked()
}

func (b *Budget) statusLocked() BudgetStatus {
	p := b.policy
	if p.MaxRequests > 0 && b.usage.Requests >= p.MaxRequests {
		return BudgetStatusExceeded
	}
	if p.MaxTokens > 0 && b.usage.Total() >= p.MaxTokens {
		return BudgetStatusExceeded
	}
	if p.Limit <= 0 {
		return BudgetStatusOK
	}
	limit := p.Limit + b.granted
	if b.spent >= limit {
		return BudgetStatusExceeded
	}
	if p.WarnAt > 0 && float64(b.spent) >= float64(limit)*p.WarnAt {
		return BudgetStatusWarn
	}
	return BudgetStatusOK
}

// Grant raises the ceiling, which is what a human approval buys. Passing zero
// applies the policy's ApprovalGrant, defaulting to the original Limit.
//
// Deliberately a grant rather than a reset: an approval that removed the budget
// entirely would turn one "yes" into an unbounded run, which is the outcome
// anyone approving a spend increase is trying to avoid.
func (b *Budget) Grant(extra Cost) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if extra <= 0 {
		extra = b.policy.ApprovalGrant
	}
	if extra <= 0 {
		extra = b.policy.Limit
	}
	b.granted += extra
}

// Spent returns the total cost recorded.
func (b *Budget) Spent() Cost {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spent
}

// Remaining returns the allowance left, or -1 when unlimited.
func (b *Budget) Remaining() Cost {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.policy.Limit <= 0 {
		return -1
	}
	rem := b.policy.Limit + b.granted - b.spent
	if rem < 0 {
		return 0
	}
	return rem
}

// Usage returns the accumulated token totals.
func (b *Budget) Usage() TokenUsage {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.usage
}

// Report is a per-model spend breakdown.
type Report struct {
	Model string
	Usage TokenUsage
	Cost  Cost
	// Currency is the ISO code Cost is denominated in, empty when the model was
	// unpriced and Cost is therefore zero for lack of a rate card rather than
	// for lack of spending.
	Currency string
}

// Breakdown returns spend per model, which is what makes an overrun
// actionable: "the run cost too much" is not, "the summariser sub-agent spent
// 80% of it" is.
func (b *Budget) Breakdown() []Report {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Report, 0, len(b.byModel))
	for model, u := range b.byModel {
		out = append(out, Report{Model: model, Usage: u, Cost: b.costs[model], Currency: b.currency[model]})
	}
	return out
}

// Exceeded reports whether any limit has been passed.
func (b *Budget) Exceeded() bool { return b.Status() == BudgetStatusExceeded }

// Err returns ErrBudgetExceeded (wrapped with the numbers) when the budget is
// spent and the policy says to stop, and nil otherwise. It is the single check
// the agent loop makes after recording usage.
func (b *Budget) Err() error {
	if b.Status() != BudgetStatusExceeded {
		return nil
	}
	if b.policy.OnExceed != BudgetStop {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return fmt.Errorf("%w: spent %s of %s after %d requests",
		ErrBudgetExceeded, b.spent, b.policy.Limit+b.granted, b.usage.Requests)
}

// ApprovalMarker builds the human-in-the-loop prompt shown when the budget is
// exhausted under BudgetRequireApproval. The numbers are in the marker so the
// person deciding sees what they are approving.
func (b *Budget) ApprovalMarker() Marker {
	b.mu.Lock()
	spent, limit, reqs := b.spent, b.policy.Limit+b.granted, b.usage.Requests
	grant := b.policy.ApprovalGrant
	if grant <= 0 {
		grant = b.policy.Limit
	}
	b.mu.Unlock()
	return Marker{
		Kind: "budget_exceeded",
		Message: fmt.Sprintf("budget exhausted: spent %s of %s over %d requests. Approve %s more?",
			spent, limit, reqs, grant),
		Meta: map[string]any{
			"spent":    spent.Float(),
			"limit":    limit.Float(),
			"grant":    grant.Float(),
			"requests": reqs,
		},
	}
}
