# 0030. Address every decision as an interrupt

- **Status:** Accepted
- **Date:** 2026-10-09
- **Source:** PR #64 (commit cc59a32), recorded as D-30 in `DESIGN_DECISIONS.md`

## Decision

Approvals, budget escalations, and clarifications use one protocol. Each `MarkerDelta` posts a `types.Interrupt` with an ID derived from the run ID, the call path, and the tool call, unique within the run, the call path from the root run, a kind, and an optional deadline.
A parent forwards a child's interrupt with the same ID and deadline, so the root stream lists every pending decision in the run and a reply by ID reaches the right child.
An unanswered interrupt never becomes a yes. With `WithInterruptExpiry` it expires to a denial by default. `fail` stops the run with `ErrInterruptExpired`. `escalate` re-posts it once to the caller one level up with a fresh deadline, then denies.
Replies are idempotent by ID and idempotency key, also after the run consumed the first reply, so a host can retry a reply safely.
A child starts with the task alone by default. `ContextFork` copies the parent branch without the delegating turn and without thinking blocks, whose signatures belong to one provider. `ContextFiltered` lets a selector choose.
A `<caller>` block tells the child its call path and that its final message is its whole result. The harness, not that text, refuses delegation to an ancestor (D-08).
