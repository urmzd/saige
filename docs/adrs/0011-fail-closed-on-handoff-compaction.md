# 0011. Fail closed on handoff compaction

- **Status:** Accepted
- **Date:** 2026-09-20
- **Source:** PR #59 (commit 1d1a557), recorded as D-11 in `DESIGN_DECISIONS.md`

## Decision

A shared summary can erase ownership boundaries and expose another owner's context.
Automatic compaction is therefore rejected for handoff groups.
A disabled policy (`none`) is accepted: it never compacts, so it cannot cross an owner's boundary.
Per-owner checkpoints and summaries must exist before this restriction can be removed.
This is a deliberate limit, not an implicit promise that long handoff sessions fit every model.
