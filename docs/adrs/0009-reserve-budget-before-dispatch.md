# 0009. Reserve budget before dispatch

- **Status:** Accepted
- **Date:** 2026-09-20
- **Source:** PR #62 (commit 9d39891), recorded as D-09 in `DESIGN_DECISIONS.md`

## Decision

A shared `Budget` reserves request, token, and cost capacity under one lock.
This prevents concurrent children from spending the same available allowance.
The host supplies per-call upper bounds. Without a bound, a call reserves the remaining allowance for that limit.
For example, four reservations of $0.25 can occupy a $1 allowance. A fifth call receives `ErrBudgetBusy`.
Settlement releases unused capacity and records actual usage once. Missing usage consumes the reservation and remains uncertain.
Local durable runs save reservations before dispatch and restore settlement receipts during replay.
This does not include a distributed account, provider-native fees, or cache storage invoices.
