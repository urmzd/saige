package types

import (
	"context"
	"errors"
	"fmt"
)

// ErrBudgetAdmission marks a rejection before the provider was called.
var ErrBudgetAdmission = errors.New("provider admission rejected")

// ErrBudgetBusy means in-flight reservations currently occupy the allowance.
var ErrBudgetBusy = errors.New("budget allowance reserved by another call")

// BudgetReceipt records the exact settlement, including conservative unknown costs.
type BudgetReceipt struct {
	ID        string
	Model     string
	Pricing   Pricing
	Usage     TokenUsage
	Cost      Cost
	Uncertain bool
	Granted   Cost
}

// BudgetReservation holds capacity until settlement. ID is its idempotency key.
type BudgetReservation struct {
	ID       string
	Cost     Cost
	Tokens   int
	Requests int
}

// Reserve atomically admits one provider attempt. PerCallCost and PerCallTokens
// must bound the configured request for parallel monetary/token admission.
// With no bound, the attempt reserves all remaining capacity for that limit.
func (b *Budget) Reserve(id string, pricing Pricing) (BudgetReservation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if id == "" {
		return BudgetReservation{}, errors.New("budget reservation needs an ID")
	}
	if b.reservations == nil {
		b.reservations = map[string]BudgetReservation{}
	}
	if _, ok := b.reservations[id]; ok {
		return BudgetReservation{}, errors.New("budget reservation ID already active")
	}
	if _, ok := b.settled[id]; ok {
		return BudgetReservation{}, errors.New("budget reservation ID already settled")
	}
	p := b.policy
	if p.Limit > 0 && pricing.IsZero() && !p.AllowUnpriced {
		return BudgetReservation{}, ErrUnpriced
	}
	r := BudgetReservation{ID: id, Requests: 1, Cost: p.PerCallCost, Tokens: p.PerCallTokens}
	var held BudgetReservation
	for _, v := range b.reservations {
		held.Cost += v.Cost
		held.Tokens += v.Tokens
		held.Requests += v.Requests
	}
	if p.PerCallCost < 0 || p.PerCallTokens < 0 {
		return r, errors.New("budget per-call bounds must not be negative")
	}
	if p.Limit > 0 && r.Cost == 0 {
		r.Cost = max(0, p.Limit+b.granted-b.spent)
	}
	if p.MaxTokens > 0 && r.Tokens == 0 {
		r.Tokens = max(0, p.MaxTokens-b.usage.Total())
	}
	if p.OnExceed != BudgetWarn {
		exceeds := (p.MaxRequests > 0 && b.usage.Requests+r.Requests > p.MaxRequests) ||
			(p.MaxTokens > 0 && (r.Tokens == 0 || b.usage.Total()+r.Tokens > p.MaxTokens)) ||
			(p.Limit > 0 && (r.Cost == 0 || b.spent+r.Cost > p.Limit+b.granted))
		if exceeds {
			return r, ErrBudgetExceeded
		}
		busy := (p.MaxRequests > 0 && b.usage.Requests+held.Requests+r.Requests > p.MaxRequests) ||
			(p.MaxTokens > 0 && b.usage.Total()+held.Tokens+r.Tokens > p.MaxTokens) ||
			(p.Limit > 0 && b.spent+held.Cost+r.Cost > p.Limit+b.granted)
		if busy {
			return r, ErrBudgetBusy
		}
	}
	b.reservations[id] = r
	return r, nil
}

// Settle releases a reservation and records known usage once. Unknown usage
// consumes the reserved cost/tokens and one request; it never becomes free.
// An underestimated bound is recorded honestly and reported as an error.
func (b *Budget) Settle(id, model string, pricing Pricing, usage TokenUsage, unknown bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.settled[id]; ok {
		return nil
	}
	r, ok := b.reservations[id]
	if !ok {
		return errors.New("unknown budget reservation")
	}
	delete(b.reservations, id)
	if unknown {
		usage.Requests = max(usage.Requests, 1)
		usage.InputTokens += max(0, r.Tokens-usage.Total())
	}
	cost := pricing.Cost(usage)
	if unknown {
		cost = max(cost, r.Cost)
		b.uncertain++
	}
	b.recordLocked(model, pricing, usage, cost)
	if b.settled == nil {
		b.settled = map[string]BudgetReceipt{}
	}
	b.settled[id] = BudgetReceipt{ID: id, Model: model, Pricing: pricing, Usage: usage, Cost: cost, Uncertain: unknown, Granted: b.granted}
	if !unknown && ((r.Cost > 0 && cost > r.Cost) || (r.Tokens > 0 && usage.Total() > r.Tokens)) {
		return fmt.Errorf("%w: provider usage exceeded the per-call reservation", ErrBudgetExceeded)
	}
	return nil
}

// RecordOnce restores a committed step's usage into a fresh budget during replay.
func (b *Budget) RecordOnce(id, model string, pricing Pricing, usage TokenUsage) (BudgetStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.settled == nil {
		b.settled = map[string]BudgetReceipt{}
	}
	if _, ok := b.settled[id]; !ok {
		b.recordLocked(model, pricing, usage, pricing.Cost(usage))
		b.settled[id] = BudgetReceipt{ID: id, Model: model, Pricing: pricing, Usage: usage, Cost: pricing.Cost(usage)}
	}
	if pricing.IsZero() && usage.Total() > 0 && b.policy.Limit > 0 && !b.policy.AllowUnpriced {
		return b.statusLocked(), ErrUnpriced
	}
	return b.statusLocked(), nil
}

// Uncertain returns the count of attempts settled without authoritative usage.
func (b *Budget) Uncertain() int { b.mu.Lock(); defer b.mu.Unlock(); return b.uncertain }

func (b *Budget) recordLocked(model string, pricing Pricing, u TokenUsage, cost Cost) {
	b.usage.Add(u)
	existing := b.byModel[model]
	existing.Add(u)
	b.byModel[model] = existing
	b.spent += cost
	b.costs[model] += cost
	if !pricing.IsZero() {
		b.currency[model] = pricing.currency()
	}
	if b.statusLocked() == BudgetStatusExceeded {
		b.breaches++
	}
}

// Receipt returns a settled value for durable persistence.
func (b *Budget) Receipt(id string) BudgetReceipt {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.settled[id]
}

// Restore applies a recorded settlement once while reconstructing a fresh run.
func (b *Budget) Restore(receipt BudgetReceipt) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if receipt.ID == "" || receipt.Cost < 0 {
		return errors.New("invalid budget receipt")
	}
	if b.settled == nil {
		b.settled = map[string]BudgetReceipt{}
	}
	if _, ok := b.settled[receipt.ID]; ok {
		return nil
	}
	b.granted = max(b.granted, receipt.Granted)
	b.recordLocked(receipt.Model, receipt.Pricing, receipt.Usage, receipt.Cost)
	if receipt.Uncertain {
		b.uncertain++
	}
	b.settled[receipt.ID] = receipt
	return nil
}

// BudgetReservationRunner saves conservative charges before provider dispatch.
// A process crash then leaves an explicit uncertain receipt for reconciliation.
type BudgetReservationRunner interface {
	RecordReservation(context.Context, string, BudgetReceipt) error
}

// ReservationReceipt captures the conservative charge if this process dies.
func (b *Budget) ReservationReceipt(id, model string, pricing Pricing) BudgetReceipt {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.reservations[id]
	usage := TokenUsage{InputTokens: r.Tokens, Requests: r.Requests}
	return BudgetReceipt{ID: id, Model: model, Pricing: pricing, Usage: usage, Cost: max(r.Cost, pricing.Cost(usage)), Uncertain: true, Granted: b.granted}
}
