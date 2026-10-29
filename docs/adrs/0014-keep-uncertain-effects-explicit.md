# 0014. Keep uncertain effects explicit

- **Status:** Accepted
- **Date:** 2026-09-20
- **Source:** PR #62 (commit 9d39891), recorded as D-14 in `DESIGN_DECISIONS.md`

## Decision

A completed step is saved before its result returns. A started step is saved before its function runs.
A crash between those records does not prove whether an external write succeeded.
Recovery therefore stops with `ErrIndeterminate`; it does not repeat the write automatically.
The host checks the external system, then supplies a result or explicitly permits retry through `Reconcile`.
For example, check a payment's idempotency key before allowing another payment attempt.
Local snapshots cannot provide an atomic transaction with an external API.
