# 0019. Rank composed gate verdicts

- **Status:** Accepted
- **Date:** 2026-10-09
- **Source:** PR #64 (commit cc59a32), recorded as D-19 in `DESIGN_DECISIONS.md`

## Decision

`Gates` runs every gate and returns the most restrictive verdict: deny, then approval, then allow.
An approval from an early gate cannot hide a later denial.
When a person edits arguments during approval, the edited call is validated and gated again.
A denial still refuses it. A second approval is not requested, because a person already decided on those arguments.
