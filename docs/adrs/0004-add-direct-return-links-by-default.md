# 0004. Add direct return links by default

- **Status:** Accepted
- **Date:** 2026-09-20
- **Source:** PR #59 (commit 1d1a557), recorded as D-04 in `DESIGN_DECISIONS.md`

## Decision

Every declared handoff edge gets a reverse edge by default.
This gives a specialist a route back when the task is outside its scope.
A strict directed policy can remove that behavior at a trust boundary.
The transfer limit remains necessary because a return edge can also create a loop.
Multiple transfers in one turn are rejected because one conversation cannot have two simultaneous owners.
