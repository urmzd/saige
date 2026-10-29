# 0017. Allow one run per branch

- **Status:** Accepted
- **Date:** 2026-10-09
- **Source:** PR #64 (commit cc59a32), recorded as D-17 in `DESIGN_DECISIONS.md`

## Decision

A run appends input, assistant turns, and tool results to the tip of its branch.
Two runs on one branch would interleave those writes and separate a `tool_use` from its `tool_result`.
`Invoke` and `RunDurable` therefore claim the branch for the length of the run. A second run on the same branch fails with `ErrRunActive`.
The claim belongs to the tree, not the `Agent`, because several agents and handoff members can share one tree.
Runs on different branches of one tree may overlap.
A message for a busy branch goes through `Submit` instead (D-25). `LoadSession` also fails while any run uses the tree.
