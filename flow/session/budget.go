package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"
	"github.com/urmzd/saige/flow/event"
)

var ErrBudget = errors.New("session: budget exceeded or not granted")

// Budget uses integer units throughout. A zero dimension is disabled; an all
// zero allowance grants no work. A positive request cap bounds unpriced models.
type Budget struct {
	CostMicros int64 `json:"cost_micros"`
	Tokens     int64 `json:"tokens"`
	Requests   int64 `json:"requests"`
}

func (b Budget) Valid() bool {
	return b.CostMicros >= 0 && b.Tokens >= 0 && b.Requests >= 0 && (b.CostMicros > 0 || b.Tokens > 0 || b.Requests > 0)
}
func (b Budget) Add(o Budget) Budget {
	sat := func(a, b int64) int64 {
		if b > 0 && a > math.MaxInt64-b {
			return math.MaxInt64
		}
		return a + b
	}
	return Budget{sat(b.CostMicros, o.CostMicros), sat(b.Tokens, o.Tokens), sat(b.Requests, o.Requests)}
}
func (b Budget) sub(o Budget) Budget {
	return Budget{b.CostMicros - o.CostMicros, b.Tokens - o.Tokens, b.Requests - o.Requests}
}
func BudgetUsage(u event.Usage) Budget {
	b := Budget{Tokens: int64(u.PromptTokens + u.CompletionTokens), Requests: int64(u.Requests)}
	if u.Cost != nil && u.Cost.Currency == "USD" {
		b.CostMicros = u.Cost.Micros
	}
	return b
}

type BudgetClaim struct {
	Allocation Budget  `json:"allocation"`
	Actual     *Budget `json:"actual,omitempty"`
}
type Account struct {
	Closed bool                   `json:"closed,omitempty"`
	Limit  Budget                 `json:"limit"`
	Claims map[string]BudgetClaim `json:"claims"`
}

func (a Account) Spent() Budget {
	var total Budget
	for _, c := range a.Claims {
		if c.Actual != nil {
			total = total.Add(*c.Actual)
		}
	}
	return total
}
func (a Account) Committed() Budget {
	var total Budget
	for _, c := range a.Claims {
		if c.Actual != nil {
			total = total.Add(*c.Actual)
		} else {
			total = total.Add(c.Allocation)
		}
	}
	return total
}
func (a *Account) Reserve(id string, want Budget) (Budget, error) {
	if id == "" || !a.Limit.Valid() {
		return Budget{}, ErrBudget
	}
	if c, ok := a.Claims[id]; ok {
		return c.Allocation, nil
	}
	if a.Closed || len(a.Claims) >= 4096 {
		return Budget{}, ErrBudget
	}
	remaining := a.Limit.sub(a.Committed())
	allocate := func(limit, remaining, wanted int64) (int64, error) {
		if wanted < 0 {
			return 0, ErrInvalid
		}
		if limit == 0 {
			return wanted, nil
		}
		if remaining <= 0 {
			return 0, ErrBudget
		}
		if wanted == 0 {
			wanted = remaining
		}
		if wanted > remaining {
			return 0, ErrBudget
		}
		return wanted, nil
	}
	var out Budget
	var err error
	if out.CostMicros, err = allocate(a.Limit.CostMicros, remaining.CostMicros, want.CostMicros); err != nil {
		return Budget{}, err
	}
	if out.Tokens, err = allocate(a.Limit.Tokens, remaining.Tokens, want.Tokens); err != nil {
		return Budget{}, err
	}
	if out.Requests, err = allocate(a.Limit.Requests, remaining.Requests, want.Requests); err != nil {
		return Budget{}, err
	}
	if !out.Valid() {
		return Budget{}, ErrBudget
	}
	if a.Claims == nil {
		a.Claims = map[string]BudgetClaim{}
	}
	a.Claims[id] = BudgetClaim{Allocation: out}
	return out, nil
}
func (a *Account) Settle(id string, actual Budget) error {
	c, ok := a.Claims[id]
	if !ok {
		return ErrNotFound
	}
	if actual.CostMicros < 0 || actual.Tokens < 0 || actual.Requests < 0 {
		return ErrInvalid
	}
	if c.Actual != nil {
		if *c.Actual != actual {
			return fmt.Errorf("%w: budget receipt changed", ErrInvalid)
		}
		return nil
	}
	c.Actual = &actual
	a.Claims[id] = c
	return nil
}

// BudgetStore serializes allocation and settlement per account, across replicas.
// Accounts are per durable run; child allocations are claims on their parent's
// account. Receipts are immutable and idempotent, including after completion.
type BudgetStore interface {
	OpenBudget(context.Context, string, string, Budget) error
	ReserveBudget(context.Context, string, string, string, Budget) (Budget, error)
	SettleBudget(context.Context, string, string, string, Budget) error
	BudgetAccount(context.Context, string, string) (Account, error)
	CloseBudget(context.Context, string, string) error
}

func (m *Memory) OpenBudget(ctx context.Context, tenant, id string, b Budget) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if tenant == "" || id == "" || !b.Valid() {
		return ErrBudget
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := scope(tenant, id)
	if a, ok := m.accounts[key]; ok {
		if a.Limit != b {
			return ErrInvalid
		}
		return nil
	}
	m.accounts[key] = Account{Limit: b, Claims: map[string]BudgetClaim{}}
	return nil
}
func (m *Memory) ReserveBudget(ctx context.Context, tenant, id, claim string, want Budget) (Budget, error) {
	if err := ctx.Err(); err != nil {
		return Budget{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := scope(tenant, id)
	a, ok := m.accounts[key]
	if !ok {
		return Budget{}, ErrNotFound
	}
	out, err := a.Reserve(claim, want)
	m.accounts[key] = a
	return out, err
}
func (m *Memory) SettleBudget(ctx context.Context, tenant, id, claim string, actual Budget) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := scope(tenant, id)
	a, ok := m.accounts[key]
	if !ok {
		return ErrNotFound
	}
	err := a.Settle(claim, actual)
	m.accounts[key] = a
	return err
}
func (m *Memory) BudgetAccount(ctx context.Context, tenant, id string) (Account, error) {
	if err := ctx.Err(); err != nil {
		return Account{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	a, ok := m.accounts[scope(tenant, id)]
	if !ok {
		return Account{}, ErrNotFound
	}
	raw, _ := json.Marshal(a)
	var out Account
	_ = json.Unmarshal(raw, &out)
	return out, nil
}

func (p *Postgres) OpenBudget(ctx context.Context, tenant, id string, b Budget) error {
	if tenant == "" || id == "" || !b.Valid() {
		return ErrBudget
	}
	raw, _ := json.Marshal(Account{Limit: b, Claims: map[string]BudgetClaim{}})
	_, err := p.pool.Exec(ctx, `INSERT INTO fiber_session_budgets(tenant,account,body) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, tenant, id, raw)
	if err != nil {
		return err
	}
	a, err := p.BudgetAccount(ctx, tenant, id)
	if err != nil {
		return err
	}
	if a.Limit != b {
		return ErrInvalid
	}
	return nil
}
func (p *Postgres) updateBudget(ctx context.Context, tenant, id string, fn func(*Account) error) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT body FROM fiber_session_budgets WHERE tenant=$1 AND account=$2 FOR UPDATE`, tenant, id).Scan(&raw); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		var a Account
		if err := json.Unmarshal(raw, &a); err != nil {
			return err
		}
		if err := fn(&a); err != nil {
			return err
		}
		raw, err := json.Marshal(a)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE fiber_session_budgets SET body=$3 WHERE tenant=$1 AND account=$2`, tenant, id, raw)
		return err
	})
}
func (p *Postgres) ReserveBudget(ctx context.Context, tenant, id, claim string, want Budget) (out Budget, err error) {
	err = p.updateBudget(ctx, tenant, id, func(a *Account) error { var err error; out, err = a.Reserve(claim, want); return err })
	return
}
func (p *Postgres) SettleBudget(ctx context.Context, tenant, id, claim string, actual Budget) error {
	return p.updateBudget(ctx, tenant, id, func(a *Account) error { return a.Settle(claim, actual) })
}
func (p *Postgres) BudgetAccount(ctx context.Context, tenant, id string) (Account, error) {
	var raw []byte
	if err := p.pool.QueryRow(ctx, `SELECT body FROM fiber_session_budgets WHERE tenant=$1 AND account=$2`, tenant, id).Scan(&raw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Account{}, ErrNotFound
		}
		return Account{}, err
	}
	var a Account
	err := json.Unmarshal(raw, &a)
	return a, err
}

// Ready reports that completion and every outstanding child receipt are durable.
func (a Account) Ready() bool {
	if !a.Closed {
		return false
	}
	for _, c := range a.Claims {
		if c.Actual == nil {
			return false
		}
	}
	return true
}
func (m *Memory) CloseBudget(ctx context.Context, tenant, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := scope(tenant, id)
	a, ok := m.accounts[key]
	if !ok {
		return ErrNotFound
	}
	a.Closed = true
	m.accounts[key] = a
	return nil
}
func (p *Postgres) CloseBudget(ctx context.Context, tenant, id string) error {
	return p.updateBudget(ctx, tenant, id, func(a *Account) error { a.Closed = true; return nil })
}
