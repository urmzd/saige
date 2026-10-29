# 0001. Keep branches separate from workers

- **Status:** Accepted
- **Date:** 2026-09-20
- **Source:** PR #59 (commit 1d1a557), recorded as D-01 in `DESIGN_DECISIONS.md`

## Decision

A branch records an alternate conversation path. It does not establish a process, tool, or database boundary.
Each subagent invocation gets a new tree. The host isolates external state when required.
This avoids a false isolation guarantee from a thread-safe tree.
