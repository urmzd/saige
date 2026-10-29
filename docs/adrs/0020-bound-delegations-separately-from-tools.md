# 0020. Bound delegations separately from tools

- **Status:** Accepted
- **Date:** 2026-10-09
- **Source:** PR #64 (commit cc59a32), recorded as D-20 in `DESIGN_DECISIONS.md`

## Decision

The parent's `ToolTimeout` bounds each tool call, including each call a child makes. It does not bound a delegation as a whole.
`SubAgentDef.Timeout` bounds one complete child run. The clock pauses while a child approval waits for a person.
A run with no consumer cannot answer approvals. A delegated child in such a run fails at once, as the parent's own tools do.
A child receives a durable approval runner only when the parent's runner resolves approvals durably.
