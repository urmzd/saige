package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/urmzd/saige/durable"
	"github.com/urmzd/saige/flow/definition"
	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/session"
	turnrt "github.com/urmzd/saige/flow/turn"
)

func allowance(b definition.Budget) session.Budget {
	return session.Budget{CostMicros: b.MaxCostMicros, Tokens: int64(b.MaxTokens), Requests: int64(b.MaxRequests)}
}
func (r *Runtime) openRunBudget(ctx context.Context, in *Input) error {
	// A host starter owns its entire tree's admission and money reservations.
	if r.sessions == nil || r.childStarter != nil {
		return nil
	}
	// Redelivery keeps the accepted allowance even if the latest definition
	// changed or removed its grant. durable also keeps that first input.
	if in.Budget == nil && r.stops != nil {
		if rec, err := r.loadRun(ctx, in.runID()); err == nil {
			var held Input
			if json.Unmarshal(rec.Input, &held) == nil && held.Trigger.Tenant == in.Trigger.Tenant && held.Subscriber == in.Subscriber {
				in.Budget = held.Budget
			}
		}
	}
	if in.Budget == nil {
		loaded, err := r.defs.Verify(ctx, in.Trigger.Tenant, in.Pin)
		if err != nil {
			return err
		}
		b := allowance(loaded.Agent.Budget)
		if !b.Valid() {
			if in.RequireBudget {
				return session.ErrBudget
			}
			return nil
		}
		in.Budget = &b
	}
	store, ok := r.sessions.(session.BudgetStore)
	if !ok {
		return fmt.Errorf("%w: store cannot enforce atomic budgets", session.ErrBudget)
	}
	return store.OpenBudget(ctx, in.Trigger.Tenant, in.runID(), *in.Budget)
}
func (r *Runtime) reserveChildBudget(ctx context.Context, parent Input, in *Input) error {
	if r.sessions == nil || r.childStarter != nil {
		return nil
	}
	if parent.Budget == nil {
		return session.ErrBudget
	}
	loaded, err := r.defs.Verify(ctx, parent.Trigger.Tenant, parent.Pin)
	if err != nil {
		return err
	}
	want := allowance(loaded.Agent.Subagents[definition.NormalizeName(in.Subscriber)])
	// Calling an agent grants neither an unbounded nor an implicit money budget.
	if !want.Valid() {
		return session.ErrBudget
	}
	store, ok := r.sessions.(session.BudgetStore)
	if !ok {
		return session.ErrBudget
	}
	granted, err := store.ReserveBudget(ctx, parent.Trigger.Tenant, parent.runID(), "child:"+in.runID(), want)
	if err != nil {
		return err
	}
	in.Budget = &granted
	return nil
}
func (r *Runtime) reserveTurnBudget(ctx context.Context, in Input, attempt int) (*turnrt.Budget, error) {
	if in.Budget == nil || r.childStarter != nil {
		return nil, nil
	}
	store, ok := r.sessions.(session.BudgetStore)
	if !ok {
		return nil, session.ErrBudget
	}
	b, err := store.ReserveBudget(ctx, in.Trigger.Tenant, in.runID(), fmt.Sprintf("attempt:%d", attempt), session.Budget{})
	if err != nil {
		return nil, durable.NonRetryable(err)
	}
	return &turnrt.Budget{CostMicros: b.CostMicros, Tokens: b.Tokens, Requests: b.Requests}, nil
}
func (r *Runtime) settleTurnBudget(ctx context.Context, in Input, attempt int, s spent, allow *turnrt.Budget, failed bool) error {
	if allow == nil {
		return nil
	}
	var t event.Tally
	// Only actual model calls settle this turn. Child totals have their own claims.
	for _, c := range s.Calls {
		t.Call(c)
	}
	actual := session.BudgetUsage(t.Usage().Total)
	// No receipt on failure is uncertain spend; retain the full reserved amount.
	if failed {
		actual = session.Budget{CostMicros: max(allow.CostMicros, actual.CostMicros), Tokens: max(allow.Tokens, actual.Tokens), Requests: max(allow.Requests, actual.Requests)}
	}
	return r.sessions.(session.BudgetStore).SettleBudget(ctx, in.Trigger.Tenant, in.runID(), fmt.Sprintf("attempt:%d", attempt), actual)
}

// closeRunBudget records completion independently of published output. A late
// descendant settles its claim and propagates through already-closed ancestors.
// No terminal event or immutable durable result is ever rewritten.
func (r *Runtime) closeRunBudget(ctx context.Context, in Input) error {
	if in.Budget == nil || r.childStarter != nil {
		return nil
	}
	store, ok := r.sessions.(session.BudgetStore)
	if !ok {
		return session.ErrBudget
	}
	// Abandoned attempts have no receipt. Completion conservatively charges
	// their full reservation; an uncertain provider call never becomes free.
	account, err := store.BudgetAccount(ctx, in.Trigger.Tenant, in.runID())
	if err != nil {
		return err
	}
	for id, claim := range account.Claims {
		if claim.Actual == nil && strings.HasPrefix(id, "attempt:") {
			if err := store.SettleBudget(ctx, in.Trigger.Tenant, in.runID(), id, claim.Allocation); err != nil {
				return err
			}
		}
	}
	if err := store.CloseBudget(ctx, in.Trigger.Tenant, in.runID()); err != nil {
		return err
	}
	for depth := 0; depth <= 32; depth++ {
		a, err := store.BudgetAccount(ctx, in.Trigger.Tenant, in.runID())
		if err != nil {
			return err
		}
		if !a.Ready() || in.Parent == nil {
			return nil
		}
		if err := store.SettleBudget(ctx, in.Trigger.Tenant, in.Parent.RunID, "child:"+in.runID(), a.Spent()); err != nil {
			return err
		}
		rec, err := r.loadRun(ctx, in.Parent.RunID)
		if err != nil {
			return err
		}
		var parent Input
		if err := json.Unmarshal(rec.Input, &parent); err != nil {
			return err
		}
		in = parent
	}
	return session.ErrInvalid
}
