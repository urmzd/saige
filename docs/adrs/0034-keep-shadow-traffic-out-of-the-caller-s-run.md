# 0034. Keep shadow traffic out of the caller's run

- **Status:** Accepted
- **Date:** 2026-10-09
- **Source:** PR #64 (commit cc59a32), recorded as D-34 in `DESIGN_DECISIONS.md`

## Decision

A shadow arm mirrors a sample of requests to another configuration to compare it.
It reserves from its own required budget, so shadow spend is never charged to, or hidden in, the primary budget.
It never executes tools: the tool calls it asks for are counted, not run.
Place the split inside the privacy decorator, so a mirrored request carries the same placeholders as the served one and personal data never reaches the shadow provider.
