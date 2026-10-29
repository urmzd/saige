# 0010. Save approvals and release local workers

- **Status:** Accepted
- **Date:** 2026-09-20
- **Source:** PR #62 (commit 9d39891), recorded as D-10 in `DESIGN_DECISIONS.md`

## Decision

Streaming approvals park a goroutine but do not hold a regular tool execution slot.
The local durable engine saves each request and returns `ErrSuspended` after the current batch drains.
Completed siblings remain saved. A later worker replays those results and resumes the approved call.
For example, an independent read can finish while a write waits for approval.
Decisions require the original run revision and an idempotency key. Changed decisions and expired requests fail.
The host authenticates users and preserves separate decisions when its interface groups approvals.
A local process lock protects one run. Remote workers still need a shared lease and fencing contract.
The `types.InterruptRouter` contract is served by the in-process router and both durable engines (`local.Engine.Router`, `anchor.Engine.Router`).
The anchor engine records each interrupt and each reply as a ledger record. A run waiting for a reply parks off the queue and holds no worker; the reply's record plus an enqueue resumes it.
Because the reply is a write-once record, a repeated reply is a no-op and a changed one is refused.
